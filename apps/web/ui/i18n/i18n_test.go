package i18n

import (
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// keyLiteral matches message keys written as string literals in templates
// and handlers. Keys must be literals so this test can see them.
var keyLiteral = regexp.MustCompile(`"((?:common|components|pages|errors)\.[A-Za-z0-9.]+)"`)

// TestUsedKeysExistInEveryLocale fails when a template or handler uses a key
// that any locale is missing.
func TestUsedKeysExistInEveryLocale(t *testing.T) {
	used := map[string][]string{} // key -> files
	root := ".."                  // apps/web/ui
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "bin" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		name := d.Name()
		if d.IsDir() || strings.HasSuffix(name, "_templ.go") || strings.HasSuffix(name, "_test.go") ||
			!(strings.HasSuffix(name, ".templ") || strings.HasSuffix(name, ".go")) {
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
	for _, l := range Locales {
		tr := Translator{Locale: l}
		for key, files := range used {
			if !tr.Has(key) {
				t.Errorf("%s: missing key %q (used in %s)", l, key, strings.Join(slices.Compact(files), ", "))
			}
		}
	}
}

// TestLocalesHaveSameKeys keeps every locale complete, not just the used keys.
func TestLocalesHaveSameKeys(t *testing.T) {
	want := Keys(DefaultLocale)
	for _, l := range Locales {
		got := Keys(l)
		for _, k := range want {
			if _, ok := slices.BinarySearch(got, k); !ok {
				t.Errorf("%s is missing %q", l, k)
			}
		}
		for _, k := range got {
			if _, ok := slices.BinarySearch(want, k); !ok {
				t.Errorf("%s has %q, which %s lacks", l, k, DefaultLocale)
			}
		}
		for _, k := range got {
			if strings.TrimSpace(Translator{Locale: l}.T(k)) == "" {
				t.Errorf("%s: %q is empty", l, k)
			}
		}
	}
}

func TestNegotiate(t *testing.T) {
	cases := []struct {
		name, cookie, accept string
		want                 Locale
	}{
		{"default", "", "", En},
		{"accept ja", "", "ja-JP,ja;q=0.9,en;q=0.8", Ja},
		{"q order wins over position", "", "en;q=0.5,ja;q=0.9", Ja},
		{"unsupported falls through", "", "fr-FR,ja;q=0.3", Ja},
		{"q=0 excluded", "", "ja;q=0,en;q=0.1", En},
		{"nothing supported", "", "fr,de", En},
		{"cookie beats header", "en", "ja", En},
		{"cookie ja", "ja", "en-US", Ja},
		{"bad cookie ignored", "xx", "ja", Ja},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if c.accept != "" {
				r.Header.Set("Accept-Language", c.accept)
			}
			if c.cookie != "" {
				r.Header.Set("Cookie", CookieName+"="+c.cookie)
			}
			if got := Negotiate(r); got != c.want {
				t.Errorf("Negotiate() = %s, want %s", got, c.want)
			}
		})
	}
}

func TestMissingKeyRendersKey(t *testing.T) {
	if got := (Translator{Locale: Ja}).T("pages.nope"); got != "pages.nope" {
		t.Fatalf("got %q", got)
	}
}
