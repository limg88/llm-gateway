package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Image is a picture sent by the client, either inline or by URL.
type Image struct {
	MediaType string
	Data      []byte
	URL       string
}

const maxImageBase64 = 20 << 20

var imageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

var errCodexImageURL = errors.New("codex does not support image URLs: send the image as a data URL")

func imagePlaceholder(n int) string { return fmt.Sprintf("[image %d]", n) }

type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	FileID   string          `json:"file_id"`
	// Anthropic image block.
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// parseContent reads a message content: a string, null, or an array of
// parts in the Chat Completions, Responses or Anthropic shape. Images are
// replaced in the text by a numbered placeholder; first is the number of
// images that came before this message.
func parseContent(raw json.RawMessage, role string, first int) (string, []Image, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil, fmt.Errorf("invalid content for role %q", role)
		}
		return s, nil, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, fmt.Errorf("invalid content for role %q", role)
	}
	var texts []string
	var imgs []Image
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text", "refusal":
			texts = append(texts, p.Text)
		case "image_url", "input_image", "image":
			n := first + len(imgs) + 1
			img, err := parseImagePart(p, n)
			if err != nil {
				return "", nil, err
			}
			imgs = append(imgs, img)
			texts = append(texts, imagePlaceholder(n))
		default:
			return "", nil, fmt.Errorf("content part type %q is not supported", p.Type)
		}
	}
	return strings.Join(texts, "\n"), imgs, nil
}

func parseImagePart(p contentPart, n int) (Image, error) {
	if p.Source != nil {
		switch p.Source.Type {
		case "base64":
			return parseDataURL("data:"+p.Source.MediaType+";base64,"+p.Source.Data, n)
		case "url":
			return imageURL(p.Source.URL, n)
		}
		return Image{}, fmt.Errorf("image %d: unsupported source type %q", n, p.Source.Type)
	}
	if p.FileID != "" {
		return Image{}, fmt.Errorf("image %d: file_id is not supported: send the image as image_url (a data URL or an http(s) URL)", n)
	}
	var url string
	raw := bytes.TrimSpace(p.ImageURL)
	if len(raw) > 0 && raw[0] == '"' {
		json.Unmarshal(raw, &url)
	} else if len(raw) > 0 {
		var o struct {
			URL string `json:"url"`
		}
		json.Unmarshal(raw, &o)
		url = o.URL
	}
	return imageURL(url, n)
}

func imageURL(url string, n int) (Image, error) {
	switch {
	case strings.HasPrefix(url, "data:"):
		return parseDataURL(url, n)
	case strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "https://"):
		return Image{URL: url}, nil
	}
	return Image{}, fmt.Errorf("image %d: image_url must be a data URL or an http(s) URL", n)
}

func parseDataURL(url string, n int) (Image, error) {
	meta, payload, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return Image{}, fmt.Errorf("image %d: the data URL must be base64-encoded", n)
	}
	declared := strings.TrimSuffix(meta, ";base64")
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(payload)); err != nil {
			return Image{}, fmt.Errorf("image %d: invalid base64 in the data URL", n)
		}
	}
	found := http.DetectContentType(data)
	if !imageTypes[found] {
		return Image{}, fmt.Errorf("image %d: unsupported image type (declared %q, found %q): use png, jpeg, gif or webp", n, declared, found)
	}
	return Image{MediaType: found, Data: data}, nil
}

// checkImages bounds the total size of the inline images.
func checkImages(imgs []Image) error {
	total := 0
	for _, img := range imgs {
		total += base64.StdEncoding.EncodedLen(len(img.Data))
	}
	if total > maxImageBase64 {
		return fmt.Errorf("the images are too large together: %d bytes of base64, at most %d (20 MB)", total, maxImageBase64)
	}
	return nil
}

// claudeContent builds the content of a Claude user message.
func claudeContent(text string, imgs []Image) any {
	type source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	}
	type block struct {
		Type   string  `json:"type"`
		Text   string  `json:"text,omitempty"`
		Source *source `json:"source,omitempty"`
	}
	blocks := []block{}
	for i, img := range imgs {
		blocks = append(blocks, block{Type: "text", Text: imagePlaceholder(i+1) + ":"})
		if img.URL != "" {
			blocks = append(blocks, block{Type: "image", Source: &source{Type: "url", URL: img.URL}})
		} else {
			blocks = append(blocks, block{Type: "image", Source: &source{
				Type: "base64", MediaType: img.MediaType, Data: base64.StdEncoding.EncodeToString(img.Data),
			}})
		}
	}
	return append(blocks, block{Type: "text", Text: text})
}

// imageFiles writes the inline images to temporary files, for the CLIs that
// take images as paths.
func imageFiles(imgs []Image) (paths []string, remove func(), err error) {
	remove = func() {
		for _, p := range paths {
			os.Remove(p)
		}
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}
	for _, img := range imgs {
		f, err := os.CreateTemp("", "llm-gateway-image-*"+ext[img.MediaType])
		if err != nil {
			remove()
			return nil, func() {}, fmt.Errorf("writing image: %w", err)
		}
		paths = append(paths, f.Name())
		_, err = f.Write(img.Data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			remove()
			return nil, func() {}, fmt.Errorf("writing image: %w", err)
		}
	}
	return paths, remove, nil
}

// imagesNote tells the model how the attached files map to the placeholders.
func imagesNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("\n\nThe %d attached image(s) are, in order, the ones the text refers to as [image 1] to [image %d].", n, n)
}
