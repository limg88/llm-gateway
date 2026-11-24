package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// The configuration uses a deliberately small TOML profile: basic strings,
// numbers, booleans and one-line arrays, tables and arrays of tables.
func configTOML(data []byte) ([]byte, error) {
	root := map[string]any{}
	table := root
	declared := map[string]bool{}
	for index, line := range strings.Split(string(data), "\n") {
		invalid := fmt.Errorf("invalid or unsupported TOML at line %d", index+1)
		quoted, escaped := false, false
		for i, c := range line {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' && quoted {
				escaped = true
				continue
			}
			if c == '"' {
				quoted = !quoted
			}
			if c == '#' && !quoted {
				line = line[:i]
				break
			}
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			array := strings.HasPrefix(line, "[[")
			left, right := "[", "]"
			if array {
				left, right = "[[", "]]"
			}
			if !strings.HasSuffix(line, right) {
				return nil, invalid
			}
			path := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, left), right))
			if path == "" {
				return nil, invalid
			}
			parent := root
			location := ""
			for {
				key, rest, err := tableKey(path)
				if err != nil {
					return nil, invalid
				}
				if rest == "" && array {
					vals, ok := parent[key].([]any)
					if parent[key] != nil && !ok {
						return nil, invalid
					}
					table = map[string]any{}
					parent[key] = append(vals, table)
					break
				}
				location += "/" + key
				if list, ok := parent[key].([]any); ok && rest != "" && len(list) > 0 {
					parent = list[len(list)-1].(map[string]any)
					location += fmt.Sprint("#", len(list))
					path = rest
					continue
				}
				if rest == "" && declared[location] {
					return nil, invalid
				}
				if parent[key] == nil {
					parent[key] = map[string]any{}
				}
				next, ok := parent[key].(map[string]any)
				if !ok {
					return nil, invalid
				}
				if rest == "" {
					table = next
					declared[location] = true
					break
				}
				parent = next
				path = rest
			}
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, invalid
		}
		key, rest, err := tableKey(strings.TrimSpace(key))
		if err != nil || rest != "" {
			return nil, invalid
		}
		if _, exists := table[key]; exists {
			return nil, invalid
		}
		var value any
		if json.Unmarshal([]byte(raw), &value) != nil || value == nil {
			return nil, invalid
		}
		switch value.(type) {
		case map[string]any:
			return nil, invalid
		}
		table[key] = value
	}
	return json.Marshal(root)
}
func tableKey(path string) (string, string, error) {
	var key string
	if strings.HasPrefix(path, "\"") {
		dec := json.NewDecoder(strings.NewReader(path))
		if err := dec.Decode(&key); err != nil {
			return "", "", err
		}
		path = path[dec.InputOffset():]
	} else {
		end := strings.IndexByte(path, '.')
		if end < 0 {
			end = len(path)
		}
		key = path[:end]
		path = path[end:]
		if !nameRE.MatchString(key) {
			return "", "", errors.New("invalid name")
		}
	}
	if path != "" && (!strings.HasPrefix(path, ".") || len(path) == 1) {
		return "", "", errors.New("invalid table path")
	}
	return key, strings.TrimPrefix(path, "."), nil
}

var childEnvNames = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "TEMP", "TMP", "SystemRoot", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}

func childEnvironment() []string {
	env := make([]string, 0, len(childEnvNames))
	for _, k := range childEnvNames {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

type Discovery struct {
	InputLimitBytes       int    `json:"input_limit_bytes,omitempty"`
	ReportInputLimitBytes int    `json:"report_input_limit_bytes,omitempty"`
	ProviderType          string `json:"provider_type,omitempty"`
}
