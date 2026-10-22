package ui_test

// Guard that is not a handler test, kept in this boundary directory on
// purpose (see AGENTS.md "Test boundaries"): templates use theme tokens.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawColor matches Tailwind palette utilities (bg-neutral-100, text-blue-600,
// border-red-300/40, ...) and dark: variants other than the icon swaps.
var (
	rawColor    = regexp.MustCompile(`\b(?:bg|text|border|ring|outline|fill|stroke|from|via|to|divide|placeholder|accent|decoration|shadow)-(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3}\b`)
	darkVariant = regexp.MustCompile(`\bdark:[\w\-\[\]/:.]+`)
)

// TestTemplatesUseThemeTokens keeps re-theming a one-file change
// (packages/uikit/theme.css): templates use the semantic color utilities,
// never raw palette classes, and dark: only to swap icons.
func TestTemplatesUseThemeTokens(t *testing.T) {
	scanned := 0
	for _, root := range []string{".", "../../../packages/uikit"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "bin" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if d.IsDir() || !templateSource(d.Name()) {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for _, m := range rawColor.FindAllString(string(src), -1) {
				t.Errorf("%s: raw palette class %q; use a theme token (packages/uikit/theme.css)", path, m)
			}
			for _, m := range darkVariant.FindAllString(string(src), -1) {
				if m != "dark:block" && m != "dark:hidden" {
					t.Errorf("%s: %q; tokens already switch with the theme, use dark: only to swap icons", path, m)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned only %d files; the walk is probably broken", scanned)
	}
}

// templateSource reports whether a file can hold class lists: templates and
// hand-written Go (generated templ output and tests are skipped).
func templateSource(name string) bool {
	if strings.HasSuffix(name, ".templ") {
		return true
	}
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_templ.go") && !strings.HasSuffix(name, "_test.go")
}
