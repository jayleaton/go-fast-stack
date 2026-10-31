package verify

import (
	"fmt"
	"strings"
)

// AllowlistProblem is why a gitleaks config's allowlist is too broad: it
// exempts a path without also requiring a value, so every credential-shaped
// value in that path (a live key in a test, a copied example env file, the
// bench script) would pass the scan. A path may only narrow a value
// exception: `paths` with `regexes` or `stopwords`, and `condition = "AND"`,
// because gitleaks ORs an allowlist's conditions by default. Value-only
// allowlists are always fine.
func AllowlistProblem(config string) string {
	for _, section := range allowlistSections(config) {
		if !allowlistHasEntries(section, "paths") {
			continue
		}
		values := allowlistHasEntries(section, "regexes") || allowlistHasEntries(section, "stopwords")
		if !values {
			return fmt.Sprintf("%s exempts `paths` without `regexes` or `stopwords`: a credential in one of those paths would pass the scan", section.name)
		}
		if !strings.EqualFold(unquote(allowlistValue(section, "condition")), "AND") {
			return fmt.Sprintf("%s exempts `paths` with values but not `condition = \"AND\"`: gitleaks ORs them, so a credential in one of those paths would pass the scan", section.name)
		}
	}
	return ""
}

// allowlistSection is one allowlist table of a gitleaks config and the lines
// after its header, up to the next table.
type allowlistSection struct {
	name  string
	lines []string
}

// allowlistSections reads the `[allowlist]`, `[rules.allowlist]` and
// `[[rules.allowlists]]` tables of a gitleaks config.
func allowlistSections(config string) []allowlistSection {
	sections := []allowlistSection{}
	current := allowlistSection{}
	flush := func() {
		if current.name != "" {
			sections = append(sections, current)
		}
	}
	for line := range strings.SplitSeq(config, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			flush()
			current = allowlistSection{name: tableName(trimmed)}
			continue
		}
		if current.name != "" && !strings.HasPrefix(trimmed, "#") {
			current.lines = append(current.lines, line)
		}
	}
	flush()
	return sections
}

// tableName is the table a header line names, or "" for one that is not an
// allowlist. `[[rules.allowlists]]` and `[[rules]]` keep their inner name.
func tableName(header string) string {
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(header, "["), "]"))
	name = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]"))
	switch name {
	case "allowlist", "rules.allowlist", "rules.allowlists":
		return name
	}
	return ""
}

// allowlistValue is the value assigned to `key` in the section: the whole
// array when it spans lines, else the rest of the line.
func allowlistValue(section allowlistSection, key string) string {
	for index := 0; index < len(section.lines); index++ {
		name, value, found := strings.Cut(strings.TrimSpace(section.lines[index]), "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		opened, closed := strings.Count(value, "["), strings.Count(value, "]")
		for opened > closed && index+1 < len(section.lines) {
			index++
			value += "\n" + section.lines[index]
			opened, closed = strings.Count(value, "["), strings.Count(value, "]")
		}
		return value
	}
	return ""
}

// allowlistHasEntries reports whether the key's array has any entry: `[]`,
// `[""]` and a missing key hold none.
func allowlistHasEntries(section allowlistSection, key string) bool {
	value := strings.TrimSpace(allowlistValue(section, key))
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	for _, char := range value {
		if strings.ContainsRune(" \t\r\n,", char) || char == '\'' || char == '"' {
			continue
		}
		return true
	}
	return false
}
