package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	mdLink  = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	bareURL = regexp.MustCompile(`https?://[^\s)\]}>"']+`)
)

// claudeSearchAction describes a WebSearch or WebFetch call of the claude CLI.
func claudeSearchAction(name string, input json.RawMessage) *WebSearchAction {
	var in struct {
		Query string `json:"query"`
		URL   string `json:"url"`
	}
	json.Unmarshal(input, &in)
	if name == "WebFetch" {
		return &WebSearchAction{Type: "open_page", URL: in.URL}
	}
	return &WebSearchAction{Type: "search", Query: in.Query}
}

// claudeSearchSources extracts the pages found by a search from its result.
func claudeSearchSources(content, structured json.RawMessage) []Source {
	var srcs []Source
	seen := map[string]bool{}
	add := func(s Source) {
		if s.URL != "" && !seen[s.URL] {
			seen[s.URL] = true
			srcs = append(srcs, s)
		}
	}
	// The structured result lists {title, url} objects at some depth.
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			if u, ok := v["url"].(string); ok && strings.HasPrefix(u, "http") {
				title, _ := v["title"].(string)
				add(Source{URL: u, Title: title})
			}
			for _, x := range v {
				walk(x)
			}
		}
	}
	var st any
	if json.Unmarshal(structured, &st) == nil {
		walk(st)
	}
	for _, s := range linksOfText(toolResultText(content)) {
		add(s)
	}
	return srcs
}

// toolResultText returns the text of a tool_result content: a string or an
// array of text blocks.
func toolResultText(content json.RawMessage) string {
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return ""
	}
	if content[0] == '"' {
		var s string
		json.Unmarshal(content, &s)
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(content, &blocks)
	var parts []string
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

func linksOfText(text string) []Source {
	var srcs []Source
	seen := map[string]bool{}
	for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
		if !seen[m[2]] {
			seen[m[2]] = true
			srcs = append(srcs, Source{URL: m[2], Title: m[1]})
		}
	}
	for _, u := range bareURL.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;:")
		if !seen[u] {
			seen[u] = true
			srcs = append(srcs, Source{URL: u})
		}
	}
	return srcs
}

// citations finds the links of an answer and reports them as url_citation
// annotations. Indexes count characters, as in the OpenAI API.
func citations(text string, sources []Source) []URLCitation {
	titles := map[string]string{}
	for _, s := range sources {
		titles[s.URL] = s.Title
	}
	var out []URLCitation
	runes, at := 0, 0
	for _, m := range mdLink.FindAllStringSubmatchIndex(text, -1) {
		runes += utf8.RuneCountInString(text[at:m[0]])
		length := utf8.RuneCountInString(text[m[0]:m[1]])
		url := text[m[4]:m[5]]
		title := text[m[2]:m[3]]
		if t := titles[url]; t != "" {
			title = t
		}
		out = append(out, URLCitation{Type: "url_citation", StartIndex: runes, EndIndex: runes + length, Title: title, URL: url})
		runes += length
		at = m[1]
	}
	return out
}

func chatAnnotations(cs []URLCitation) []ChatAnnotation {
	var out []ChatAnnotation
	for _, c := range cs {
		a := ChatAnnotation{Type: "url_citation"}
		a.URLCitation.StartIndex, a.URLCitation.EndIndex = c.StartIndex, c.EndIndex
		a.URLCitation.Title, a.URLCitation.URL = c.Title, c.URL
		out = append(out, a)
	}
	return out
}

func searchSources(searches []SearchEvent) []Source {
	var out []Source
	for _, s := range searches {
		out = append(out, s.Sources...)
	}
	return out
}
