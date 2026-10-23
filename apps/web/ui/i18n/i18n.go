// Package i18n loads the web UI messages (ported from the original
// apps/web-app/messages/*.json) and resolves the request locale.
//
// The messages are flat strings addressed by dotted keys ("pages.login.title"),
// optionally with named placeholders ("{email}", filled by Tf). They contain no
// plurals, so a map lookup is all we need; add a library such as go-i18n only
// when a message needs plural rules.
package i18n

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Locale is a supported language tag.
type Locale string

const (
	En Locale = "en"
	Ja Locale = "ja"

	// DefaultLocale is used when neither the cookie nor Accept-Language match.
	DefaultLocale = En

	// CookieName stores the locale chosen in the language switcher.
	CookieName = "locale"
)

// Locales lists every supported locale in switcher order.
var Locales = []Locale{En, Ja}

//go:embed messages/*.json
var messagesFS embed.FS

var catalogs = mustLoad()

func mustLoad() map[Locale]map[string]string {
	out := make(map[Locale]map[string]string, len(Locales))
	for _, l := range Locales {
		raw, err := messagesFS.ReadFile("messages/" + string(l) + ".json")
		if err != nil {
			panic(fmt.Sprintf("i18n: read %s: %v", l, err))
		}
		var tree map[string]any
		if err := json.Unmarshal(raw, &tree); err != nil {
			panic(fmt.Sprintf("i18n: parse %s: %v", l, err))
		}
		flat := map[string]string{}
		flatten("", tree, flat)
		out[l] = flat
	}
	return out
}

func flatten(prefix string, node map[string]any, out map[string]string) {
	for k, v := range node {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch v := v.(type) {
		case string:
			out[key] = v
		case map[string]any:
			flatten(key, v, out)
		default:
			panic(fmt.Sprintf("i18n: key %q has unsupported type %T", key, v))
		}
	}
}

// Keys returns the sorted message keys of a locale.
func Keys(l Locale) []string {
	keys := make([]string, 0, len(catalogs[l]))
	for k := range catalogs[l] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Supported reports whether s names a supported locale.
func Supported(s string) (Locale, bool) {
	for _, l := range Locales {
		if string(l) == s {
			return l, true
		}
	}
	return "", false
}

// Translator resolves keys for one locale.
type Translator struct {
	Locale Locale
}

// T returns the message for key. A missing key renders as the key itself, which
// the render tests treat as a failure.
func (t Translator) T(key string) string {
	if v, ok := catalogs[t.Locale][key]; ok {
		return v
	}
	return key
}

// Tf is T with named placeholders: Tf("k", "email", e) fills "{email}".
func (t Translator) Tf(key string, pairs ...string) string {
	msg := t.T(key)
	for i := 0; i+1 < len(pairs); i += 2 {
		msg = strings.ReplaceAll(msg, "{"+pairs[i]+"}", pairs[i+1])
	}
	return msg
}

// Has reports whether key exists in the translator's locale.
func (t Translator) Has(key string) bool {
	_, ok := catalogs[t.Locale][key]
	return ok
}

// Label returns the display name of a locale in the translator's language.
func (t Translator) Label(l Locale) string {
	switch l {
	case En:
		return t.T("common.english")
	case Ja:
		return t.T("common.japanese")
	}
	return string(l)
}

// Date formats a calendar date the way each locale writes it.
func (t Translator) Date(d time.Time) string {
	if t.Locale == Ja {
		return d.Format("2006年1月2日")
	}
	return d.Format("Jan 2, 2006")
}

// DateTime formats a date with a 24h time.
func (t Translator) DateTime(d time.Time) string {
	if t.Locale == Ja {
		return d.Format("2006年1月2日 15:04 MST")
	}
	return d.Format("Jan 2, 2006 15:04 MST")
}

type ctxKey struct{}

// WithTranslator stores the translator for templates rendered with ctx.
func WithTranslator(ctx context.Context, t Translator) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// From returns the request translator, falling back to the default locale.
func From(ctx context.Context) Translator {
	if t, ok := ctx.Value(ctxKey{}).(Translator); ok {
		return t
	}
	return Translator{Locale: DefaultLocale}
}

// T is the shorthand templates use: { i18n.T(ctx, "pages.login.title") }.
func T(ctx context.Context, key string) string {
	return From(ctx).T(key)
}

// Tf is the template shorthand for Translator.Tf.
func Tf(ctx context.Context, key string, pairs ...string) string {
	return From(ctx).Tf(key, pairs...)
}

// Negotiate picks the locale for a request: the switcher cookie first, then the
// highest-weighted supported Accept-Language entry, then DefaultLocale.
func Negotiate(r *http.Request) Locale {
	if c, err := r.Cookie(CookieName); err == nil {
		if l, ok := Supported(c.Value); ok {
			return l
		}
	}
	return fromAcceptLanguage(r.Header.Get("Accept-Language"))
}

func fromAcceptLanguage(header string) Locale {
	type candidate struct {
		lang string
		q    float64
	}
	var cands []candidate
	for part := range strings.SplitSeq(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if tag == "" {
			continue
		}
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		base, _, _ := strings.Cut(strings.ToLower(tag), "-")
		cands = append(cands, candidate{base, q})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].q > cands[j].q })
	for _, c := range cands {
		if c.q <= 0 {
			continue
		}
		if l, ok := Supported(c.lang); ok {
			return l
		}
	}
	return DefaultLocale
}

// cookieMaxAge keeps the switcher choice for a year, in seconds.
const cookieMaxAge = 60 * 60 * 24 * 365

// SetCookie persists the switcher choice for a year.
func SetCookie(w http.ResponseWriter, l Locale, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    string(l),
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true, // only the server reads it
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}
