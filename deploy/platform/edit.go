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
	for _, entry := range keys {
		text := render(entry)
		found := false
		for index := 0; index+1 < len(root.Content); index += 2 {
			key, val := root.Content[index], root.Content[index+1]
			if key.Value != entry.Name {
				continue
			}
			found = true
			if val.Kind == yaml.ScalarNode && val.Value == entry.Value {
				break
			}
			if val.Line != key.Line || val.Line > len(lines) {
				return nil, nil, fmt.Errorf("platform.yaml: put %s on one line (e.g. %s: %s) to set it automatically", entry.Name, entry.Name, text)
			}
			line := lines[val.Line-1]
			start := val.Column - 1
			rest := line[start:]
			end := len(strings.TrimRight(rest, "\r\n"))
			if count := strings.Index(rest, " #"); count >= 0 && count < end {
				end = count
			}
			lines[val.Line-1] = line[:start] + text + rest[end:]
			changed = append(changed, entry.Name)
			break
		}
		if !found {
			if count := len(lines); count > 0 && !strings.HasSuffix(lines[count-1], "\n") {
				lines[count-1] += "\n"
			}
			lines = append(lines, entry.Name+": "+text+"\n")
			changed = append(changed, entry.Name)
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
func render(key Key) string {
	if key.Name == "admin" {
		return key.Value
	}
	var node yaml.Node
	if key.Value != "" && yaml.Unmarshal([]byte("v: "+key.Value), &node) == nil && len(node.Content) == 1 &&
		len(node.Content[0].Content) == 2 && node.Content[0].Content[1].Kind == yaml.ScalarNode &&
		node.Content[0].Content[1].Tag == "!!str" && node.Content[0].Content[1].Value == key.Value {
		return key.Value
	}
	return strconv.Quote(key.Value)
}
