package ui_test

// Guards that are not handler tests, kept in this boundary directory on
// purpose (see AGENTS.md "Test boundaries"): every template key exists in
// every locale, the embedded Datastar bundle matches its pinned hash, and
// templates use theme tokens instead of raw palette classes.

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/static"
)

// keyLiteral matches message keys written as string literals in templates
// and handlers. i18n.Keys must be literals so this test can see them.
var keyLiteral = regexp.MustCompile(`"((?:common|components|pages|errors)\.[A-Za-z0-9.]+)"`)

// TestUsedKeysExistInEveryLocale fails when a template or handler uses a key
// that any locale is missing.
func TestUsedKeysExistInEveryLocale(t *testing.T) {
	t.Parallel()
	used := map[string][]string{} // key -> files
	root := "."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "bin" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		name := d.Name()
		if d.IsDir() || strings.HasSuffix(name, "_templ.go") || strings.HasSuffix(name, "_test.go") ||
			(!strings.HasSuffix(name, ".templ") && !strings.HasSuffix(name, ".go")) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range keyLiteral.FindAllStringSubmatch(string(src), -1) {
			used[m[1]] = append(used[m[1]], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(used) < 50 {
		t.Fatalf("found only %d keys in templates; the scan is probably broken", len(used))
	}
	for _, l := range i18n.Locales {
		tr := i18n.Translator{Locale: l}
		for key, files := range used {
			if !tr.Has(key) {
				t.Errorf("%s: missing key %q (used in %s)", l, key, strings.Join(slices.Compact(files), ", "))
			}
		}
	}
}

// TestLocalesHaveSameKeys keeps every locale complete, not just the used keys.
func TestLocalesHaveSameKeys(t *testing.T) {
	t.Parallel()
	want := i18n.Keys(i18n.DefaultLocale)
	for _, l := range i18n.Locales {
		got := i18n.Keys(l)
		for _, k := range want {
			if _, ok := slices.BinarySearch(got, k); !ok {
				t.Errorf("%s is missing %q", l, k)
			}
		}
		for _, k := range got {
			if _, ok := slices.BinarySearch(want, k); !ok {
				t.Errorf("%s has %q, which %s lacks", l, k, i18n.DefaultLocale)
			}
		}
		for _, k := range got {
			if strings.TrimSpace(i18n.Translator{Locale: l}.T(k)) == "" {
				t.Errorf("%s: %q is empty", l, k)
			}
		}
	}
}

func TestNegotiate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, cookie, accept string
		want                 i18n.Locale
	}{
		{"default", "", "", i18n.En},
		{"accept ja", "", "ja-JP,ja;q=0.9,en;q=0.8", i18n.Ja},
		{"q order wins over position", "", "en;q=0.5,ja;q=0.9", i18n.Ja},
		{"unsupported falls through", "", "fr-FR,ja;q=0.3", i18n.Ja},
		{"q=0 excluded", "", "ja;q=0,en;q=0.1", i18n.En},
		{"nothing supported", "", "fr,de", i18n.En},
		{"cookie beats header", "en", "ja", i18n.En},
		{"cookie ja", "ja", "en-US", i18n.Ja},
		{"bad cookie ignored", "xx", "ja", i18n.Ja},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.accept != "" {
				r.Header.Set("Accept-Language", c.accept)
			}
			if c.cookie != "" {
				r.Header.Set("Cookie", i18n.CookieName+"="+c.cookie)
			}
			if got := i18n.Negotiate(r); got != c.want {
				t.Errorf("i18n.Negotiate() = %s, want %s", got, c.want)
			}
		})
	}
}

func TestMissingKeyRendersKey(t *testing.T) {
	t.Parallel()
	if got := (i18n.Translator{Locale: i18n.Ja}).T("pages.nope"); got != "pages.nope" {
		t.Fatalf("got %q", got)
	}
}

func TestDatastarPin(t *testing.T) {
	t.Parallel()
	b, err := static.FS.ReadFile(static.DatastarFile)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != static.DatastarSHA256 {
		t.Fatalf("%s sha256 = %s, pinned %s", static.DatastarFile, got, static.DatastarSHA256)
	}
	if !strings.HasPrefix(string(b), "// Datastar v"+static.DatastarVersion+"\n") {
		t.Fatalf("%s header does not say v%s", static.DatastarFile, static.DatastarVersion)
	}
}

func TestHandlerCaching(t *testing.T) {
	t.Parallel()
	h := static.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, static.URL(static.CSSFile), nil))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned: code %d cache %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), "tailwindcss") {
		t.Fatal("app.css is not the Tailwind build")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/"+static.CSSFile+"?v=stale", nil))
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("stale version cached: %q", rec.Header().Get("Cache-Control"))
	}
}

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
