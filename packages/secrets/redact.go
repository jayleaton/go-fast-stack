package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// RedactOptions configures NewRedactingHandler.
type RedactOptions struct {
	// KeepDevMailLinks leaves the "url" attribute of the dev mailer's records
	// (packages/auth DevMailer: message "auth: … (dev mode, not sent)")
	// readable, so developers can copy verification, reset and invite links
	// from the log (AGENTS.md "Dev loop"). Every other attribute of those
	// records is still redacted. Enable it only outside production.
	KeepDevMailLinks bool
}

// NewRedactingHandler wraps h so no record reaches it with a credential in
// it. Both binaries build their logger with it. For the message and every
// attribute, through groups, after resolving slog.LogValuer and using the
// Error() text of errors (and String() of fmt.Stringer values):
//
//   - the value of an attribute whose key names a credential becomes
//     "[redacted]" (see sensitiveKey for the list); empty values and
//     Fingerprint() output ("fp:…", "none") stay readable;
//   - inside any string: "Bearer x", URL userinfo passwords
//     (postgres://u:[redacted]@h), values of token/code/secret/key/password/
//     signature-like query or key=value parameters, the token segment of
//     emailed auth links (/reset-password/[redacted]), and Cookie,
//     Set-Cookie and Authorization header lines.
//
// It is a safety net: code still logs Fingerprint(), never a value.
func NewRedactingHandler(h slog.Handler, opts RedactOptions) slog.Handler {
	return &redactingHandler{next: h, opts: opts}
}

type redactingHandler struct {
	next slog.Handler
	opts RedactOptions
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	keepURL := h.opts.KeepDevMailLinks && isDevMailMessage(r.Message)
	out := slog.NewRecord(r.Time, r.Level, RedactString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if keepURL && a.Key == devMailLinkKey {
			out.AddAttrs(a)
		} else {
			out.AddAttrs(redactAttr(a))
		}
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(red), opts: h.opts}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), opts: h.opts}
}

// The dev mailer (packages/auth/mailer.go) logs "auth: <kind> email (dev
// mode, not sent)" with the link under "url". The key alone is too generic
// to exempt, so the exception needs both.
const devMailLinkKey = "url"

func isDevMailMessage(msg string) bool {
	return strings.HasPrefix(msg, "auth: ") && strings.HasSuffix(msg, "(dev mode, not sent)")
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		if sensitiveKey(a.Key) {
			return slog.String(a.Key, redacted)
		}
		attrs := v.Group()
		red := make([]slog.Attr, len(attrs))
		for i, g := range attrs {
			red[i] = redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(red...)}
	}
	if sensitiveKey(a.Key) {
		if s := v.String(); v.Kind() == slog.KindString && (s == "" || s == redacted || isFingerprint(s)) {
			return slog.Attr{Key: a.Key, Value: v}
		}
		return slog.String(a.Key, redacted)
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactString(v.String()))
	case slog.KindAny:
		if g, ok := mapGroup(v.Any()); ok {
			return redactAttr(slog.Attr{Key: a.Key, Value: g})
		}
		s, ok := textOf(v.Any())
		if !ok {
			s, ok = jsonOf(v.Any())
		}
		if ok {
			if red := RedactString(s); red != s {
				return slog.String(a.Key, red)
			}
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// mapGroup turns a map with string keys (http.Header, url.Values,
// map[string]string) into a group, so each entry is redacted by its key.
// Single-value slices log as their value.
func mapGroup(x any) (slog.Value, bool) {
	rv := reflect.ValueOf(x)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return slog.Value{}, false
	}
	keys := rv.MapKeys()
	slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
	attrs := make([]slog.Attr, 0, len(keys))
	for _, k := range keys {
		e := rv.MapIndex(k)
		if e.Kind() == reflect.Slice && e.Len() == 1 {
			e = e.Index(0)
		}
		attrs = append(attrs, slog.Any(k.String(), e.Interface()))
	}
	return slog.GroupValue(attrs...), true
}

// jsonOf is the JSON a handler would print for a struct (or pointer to
// one), so credential fields inside it ("token":"…") are seen.
func jsonOf(x any) (string, bool) {
	rv := reflect.ValueOf(x)
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return "", false
	}
	b, err := json.Marshal(x)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// textOf returns the text a handler would print for errors, Stringers and
// string slices; ok is false for anything else.
func textOf(x any) (s string, ok bool) {
	defer func() {
		if recover() != nil { // a nil-pointer String(): leave it to the handler
			s, ok = "", false
		}
	}()
	switch t := x.(type) {
	case error:
		return t.Error(), true
	case fmt.Stringer:
		return t.String(), true
	case []string:
		return fmt.Sprint(t), true
	}
	return "", false
}

func isFingerprint(s string) bool {
	if s == "none" {
		return true
	}
	h, ok := strings.CutPrefix(s, "fp:")
	return ok && len(h) == 8 && strings.Trim(h, "0123456789abcdef") == ""
}

// Words in an attribute key that name a credential. A key is split into
// lower-case words at "-", "_", ".", spaces and camelCase boundaries
// ("Set-Cookie" → set, cookie; "sessionToken" → session, token), so
// "status", "code", "keys" or "secrets_provider" stay readable.
var (
	sensitiveWords = map[string]bool{
		"password": true, "passwd": true, "passphrase": true, "pwd": true,
		"secret": true, "token": true, "apikey": true, "authorization": true,
		"cookie": true, "dsn": true, "otp": true, "totp": true,
		"credential": true, "credentials": true, "signature": true, "privatekey": true,
	}
	// Adjacent word pairs: api_key, private_key, database_url, db_url,
	// backup_codes, recovery_code, access_key.
	sensitivePairs = map[[2]string]bool{
		{"api", "key"}: true, {"private", "key"}: true, {"access", "key"}: true,
		{"database", "url"}: true, {"db", "url"}: true,
		{"backup", "code"}: true, {"backup", "codes"}: true,
		{"recovery", "code"}: true, {"recovery", "codes"}: true,
	}
)

// sensitiveKey reports whether an attribute key names a credential.
func sensitiveKey(key string) bool {
	words := keyWords(key)
	for i, w := range words {
		if sensitiveWords[w] || (i > 0 && sensitivePairs[[2]string{words[i-1], w}]) {
			return true
		}
	}
	return false
}

func keyWords(key string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(key)
	for i, r := range rs {
		switch {
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			flush()
			continue
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) ||
			(unicode.IsUpper(rs[i-1]) && i+1 < len(rs) && unicode.IsLower(rs[i+1]))):
			flush() // fooBar → foo|Bar, HTTPHeader → HTTP|Header
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

// Patterns RedactString applies, in order.
var stringRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Header lines: everything after the colon.
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie|(?:proxy-)?authorization|x-api-key)(\s*:\s*)[^\r\n]+`), "${1}${2}" + redacted},
	// Bearer tokens in any string: Authorization values, error texts.
	{regexp.MustCompile(`(?i)\b(bearer)(\s+)[A-Za-z0-9\-._~+/]+=*`), "${1}${2}" + redacted},
	// URL userinfo password: scheme://user:pass@host.
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://[^\s:/@]*):[^\s/?#]+@`), "${1}:" + redacted + "@"},
	// Token path segment of emailed auth links.
	{regexp.MustCompile(`(?i)(/(?:reset-password|verify-email|set-password|magic-link|accept-invite|invite)/)[^/?#\s"'<>]+`), "${1}" + redacted},
	// "name":"value" in JSON where the name looks like a credential.
	{regexp.MustCompile(`(?i)("[A-Za-z0-9_.\-]*(?:token|secret|password|passwd|apikey|api_key|authorization|cookie|credential)[A-Za-z0-9_.\-]*"\s*:\s*")(?:[^"\\]|\\.)*"`), "${1}" + redacted + `"`},
	// name=value where the name looks like a credential: query strings,
	// cookie pairs, "token=abc" in messages.
	{regexp.MustCompile(`(?i)((?:^|[?&;,\s])[A-Za-z0-9_.\-]*(?:token|code|secret|key|pass|pwd|signature|sig|otp|auth|credential|session)[A-Za-z0-9_.\-]*=)[^&#;,\s"'<>]+`), "${1}" + redacted},
}

// RedactString applies the string rules of NewRedactingHandler to s.
func RedactString(s string) string {
	if len(s) < 4 {
		return s
	}
	for _, r := range stringRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
