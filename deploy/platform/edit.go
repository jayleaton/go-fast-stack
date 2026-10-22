package platform

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Key is a platform.yaml key Edit can set.
type Key struct{ Name, Value string }

// Edit sets top-level keys in platform.yaml source and validates the
// result. A value on the key's own line is replaced in place, so comments,
// blank lines and the layout of everything else stay byte for byte; a
// missing key is appended. It returns the new source and the names of the
// keys whose value changed (none: raw is returned as is).
func Edit(raw []byte, keys []Key) ([]byte, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("platform.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("platform.yaml: expected a mapping")
	}
	root := doc.Content[0]
	lines := strings.SplitAfter(string(raw), "\n")
	var changed []string
	for _, k := range keys {
		text := render(k)
		found := false
		for i := 0; i+1 < len(root.Content); i += 2 {
			key, val := root.Content[i], root.Content[i+1]
			if key.Value != k.Name {
				continue
			}
			found = true
			if val.Kind == yaml.ScalarNode && val.Value == k.Value {
				break
			}
			if val.Line != key.Line || val.Line > len(lines) {
				return nil, nil, fmt.Errorf("platform.yaml: put %s on one line (e.g. %s: %s) to set it automatically", k.Name, k.Name, text)
			}
			line := lines[val.Line-1]
			start := val.Column - 1
			rest := line[start:]
			end := len(strings.TrimRight(rest, "\r\n"))
			if c := strings.Index(rest, " #"); c >= 0 && c < end {
				end = c
			}
			lines[val.Line-1] = line[:start] + text + rest[end:]
			changed = append(changed, k.Name)
			break
		}
		if !found {
			if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
				lines[n-1] += "\n"
			}
			lines = append(lines, k.Name+": "+text+"\n")
			changed = append(changed, k.Name)
		}
	}
	if len(changed) == 0 {
		return raw, nil, nil
	}
	out := []byte(strings.Join(lines, ""))
	if _, err := Parse(out); err != nil {
		return nil, nil, err
	}
	return out, changed, nil
}

// render is a value as YAML text: booleans bare, strings plain when that
// reads back as the same string, else double-quoted.
func render(k Key) string {
	if k.Name == "admin" {
		return k.Value
	}
	var n yaml.Node
	if k.Value != "" && yaml.Unmarshal([]byte("v: "+k.Value), &n) == nil && len(n.Content) == 1 &&
		len(n.Content[0].Content) == 2 && n.Content[0].Content[1].Kind == yaml.ScalarNode &&
		n.Content[0].Content[1].Tag == "!!str" && n.Content[0].Content[1].Value == k.Value {
		return k.Value
	}
	return strconv.Quote(k.Value)
}
