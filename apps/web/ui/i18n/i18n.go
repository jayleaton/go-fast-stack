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
	for _, locale := range Locales {
		raw, err := messagesFS.ReadFile("messages/" + string(locale) + ".json")
		if err != nil {
			panic(fmt.Sprintf("i18n: read %s: %v", locale, err))
		}
		var tree map[string]any
		if err := json.Unmarshal(raw, &tree); err != nil {
			panic(fmt.Sprintf("i18n: parse %s: %v", locale, err))
		}
		flat := map[string]string{}
		flatten("", tree, flat)
		out[locale] = flat
	}
	return out
}

func flatten(prefix string, node map[string]any, out map[string]string) {
	for name, value := range node {
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		switch value := value.(type) {
		case string:
			out[key] = value
		case map[string]any:
			flatten(key, value, out)
		default:
			panic(fmt.Sprintf("i18n: key %q has unsupported type %T", key, value))
		}
	}
}

// Keys returns the sorted message keys of a locale.
func Keys(locale Locale) []string {
	keys := make([]string, 0, len(catalogs[locale]))
	for key := range catalogs[locale] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Supported reports whether s names a supported locale.
func Supported(text string) (Locale, bool) {
	for _, locale := range Locales {
		if string(locale) == text {
			return locale, true
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
func (translator Translator) Translate(key string) string {
	if value, ok := catalogs[translator.Locale][key]; ok {
		return value
	}
	return key
}

// Tf is T with named placeholders: Tf("k", "email", e) fills "{email}".
func (translator Translator) Tf(key string, pairs ...string) string {
	msg := translator.Translate(key)
	for index := 0; index+1 < len(pairs); index += 2 {
		msg = strings.ReplaceAll(msg, "{"+pairs[index]+"}", pairs[index+1])
	}
	return msg
}

// Has reports whether key exists in the translator's locale.
func (translator Translator) Has(key string) bool {
	_, ok := catalogs[translator.Locale][key]
	return ok
}

// Label returns the display name of a locale in the translator's language.
func (translator Translator) Label(locale Locale) string {
	switch locale {
	case En:
		return translator.Translate("common.english")
	case Ja:
		return translator.Translate("common.japanese")
	}
	return string(locale)
}

// Date formats a calendar date the way each locale writes it.
func (translator Translator) Date(moment time.Time) string {
	if translator.Locale == Ja {
		return moment.Format("2006年1月2日")
	}
	return moment.Format("Jan 2, 2006")
}

// DateTime formats a date with a 24h time.
func (translator Translator) DateTime(moment time.Time) string {
	if translator.Locale == Ja {
		return moment.Format("2006年1月2日 15:04 MST")
	}
	return moment.Format("Jan 2, 2006 15:04 MST")
}

type ctxKey struct{}

// WithTranslator stores the translator for templates rendered with ctx.
func WithTranslator(ctx context.Context, translator Translator) context.Context {
	return context.WithValue(ctx, ctxKey{}, translator)
}

// From returns the request translator, falling back to the default locale.
func From(ctx context.Context) Translator {
	if translator, ok := ctx.Value(ctxKey{}).(Translator); ok {
		return translator
	}
	return Translator{Locale: DefaultLocale}
}

// T is the shorthand templates use: { i18n.T(ctx, "pages.login.title") }.
func Translate(ctx context.Context, key string) string {
	return From(ctx).Translate(key)
}

// Tf is the template shorthand for Translator.Tf.
func Tf(ctx context.Context, key string, pairs ...string) string {
	return From(ctx).Tf(key, pairs...)
}

// Negotiate picks the locale for a request: the switcher cookie first, then the
// highest-weighted supported Accept-Language entry, then DefaultLocale.
func Negotiate(request *http.Request) Locale {
	if cookie, err := request.Cookie(CookieName); err == nil {
		if locale, ok := Supported(cookie.Value); ok {
			return locale
		}
	}
	return fromAcceptLanguage(request.Header.Get("Accept-Language"))
}

func fromAcceptLanguage(header string) Locale {
	type candidate struct {
		lang    string
		quality float64
	}
	var cands []candidate
	for part := range strings.SplitSeq(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if tag == "" {
			continue
		}
		quality := 1.0
		if value, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if parsed, err := strconv.ParseFloat(value, 64); err == nil {
				quality = parsed
			}
		}
		base, _, _ := strings.Cut(strings.ToLower(tag), "-")
		cands = append(cands, candidate{base, quality})
	}
	sort.SliceStable(cands, func(index, inner int) bool { return cands[index].quality > cands[inner].quality })
	for _, candidate := range cands {
		if candidate.quality <= 0 {
			continue
		}
		if locale, ok := Supported(candidate.lang); ok {
			return locale
		}
	}
	return DefaultLocale
}

// cookieMaxAge keeps the switcher choice for a year, in seconds.
const cookieMaxAge = 60 * 60 * 24 * 365

// SetCookie persists the switcher choice for a year.
func SetCookie(writer http.ResponseWriter, locale Locale, secure bool) {
	http.SetCookie(writer, &http.Cookie{
		Name:     CookieName,
		Value:    string(locale),
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true, // only the server reads it
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}
