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
	"unicode/utf8"
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
//     "[redacted]" (see credentialName for the rule and word lists); empty
//     values, booleans and Fingerprint() output ("fp:…", "none") stay
//     readable;
//   - inside any string: "Bearer x", URL userinfo passwords
//     (postgres://u:[redacted]@h), the value of every JSON "name":"value"
//     and query, cookie or message name=value pair whose name is a
//     credential (?code=, ?key=, sig=, sid= and session= included), the
//     token segment of emailed auth links (/reset-password/[redacted]), and
//     Cookie, Set-Cookie and Authorization header lines.
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
			out.AddAttrs(redactAttr(a, false))
		}
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a, false)
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

// redactAttr redacts a by its key and value. wire marks keys that came from
// a map (headers, query values), named like request parameters.
func redactAttr(a slog.Attr, wire bool) slog.Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		if credentialName(a.Key, wire) {
			return slog.String(a.Key, redacted)
		}
		attrs := v.Group()
		red := make([]slog.Attr, len(attrs))
		for i, g := range attrs {
			red[i] = redactAttr(g, wire)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(red...)}
	}
	if credentialName(a.Key, wire) {
		if v.Kind() == slog.KindBool || (v.Kind() == slog.KindString && keepValue(v.String())) {
			return slog.Attr{Key: a.Key, Value: v} // has_password=true carries no credential
		}
		return slog.String(a.Key, redacted)
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactString(v.String()))
	case slog.KindAny:
		if g, ok := mapGroup(v.Any()); ok {
			entries := g.Group()
			for i, e := range entries {
				entries[i] = redactAttr(e, true)
			}
			return slog.Attr{Key: a.Key, Value: slog.GroupValue(entries...)}
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
// map[string]string) into a group, so each entry is redacted by its key,
// read like a request parameter name (a bare "code" or "key" counts).
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

// keepValue reports whether a value under a credential name may stay
// readable: it is empty, already redacted, or a Fingerprint().
func keepValue(s string) bool {
	return s == "" || s == redacted || isFingerprint(s)
}

// Credential names. Every name the handler sees (attribute keys, map keys,
// JSON fields, the name in name=value) is split into lower-case words at
// "-", "_", ".", spaces and camelCase boundaries ("Set-Cookie" → set,
// cookie; "sessionToken" → session, token). A name is a credential when it
// has a head (a word or word pair below) and no metadata word comes after
// the last head: "new_password", "password_hash" and "refresh_token" are
// credentials; "set_password_page", "token_ttl" and "api_key_fingerprint"
// are facts about one. Words that merely contain a head ("passed",
// "keyboard", "author", "tokens") are not heads.
var (
	// A word that is, or ends in, one of these is a head, so run-together
	// names count too: "csrftoken", "pgpassword", "clientsecret", "jsessionid".
	headSuffixes = []string{
		"password", "passwd", "passphrase", "secret", "token", "apikey", "authorization",
		"cookie", "credential", "credentials", "signature", "privatekey", "accesskey",
		"secretkey", "sessionid",
	}
	// Short heads, matched as whole words only ("bypass" is not "pass").
	headWords = map[string]bool{
		"pwd": true, "pass": true, "dsn": true, "otp": true, "totp": true, "jwt": true,
		"phpsessid": true, "authcode": true, "authkey": true,
	}
	// Words that are a head only after one of the listed words: "api_key",
	// "recovery_code", "connect.sid", but not "cache_key", "status_code" or
	// "exit_code".
	headPairs = map[string]map[string]bool{
		"key": set("api", "private", "access", "secret", "signing", "encryption", "hmac",
			"master", "client", "license", "session", "auth", "app", "webhook", "shared"),
		"keys": set("api", "private", "access", "secret", "signing", "encryption"),
		"code": set("auth", "oauth", "verification", "verify", "confirmation", "activation",
			"invite", "invitation", "reset", "login", "signin", "access", "security", "mfa",
			"2fa", "sms", "email", "magic", "onetime", "backup", "recovery"),
		"codes":    set("backup", "recovery", "verification", "mfa"),
		"url":      set("database", "db"),
		"verifier": set("code", "oauth"), // PKCE code_verifier, OAuth 1 oauth_verifier
		"auth":     set("x", "basic", "proxy", "http"),
		"header":   set("auth"),
		"session":  set("host", "secure"), // __Host-session, __Secure-session
		"sid":      set("connect", "host", "secure"),
	}
	// Words that are a credential when they are the whole name. In a query
	// string or cookie ("?code=", "?key=", "sig=", "session=", "sid=") they
	// all are; as an attribute or JSON key only those marked true, since a
	// logged "code", "key" or "session" is an error code, a cache key or a
	// session record (whose own fields are redacted by name).
	bareHeads = map[string]bool{
		"code": false, "key": false, "session": false, "sig": true, "auth": true, "sid": true,
	}
	// Metadata: after the last head, any of these makes the name describe
	// the credential rather than hold it ("password_min_length",
	// "token_expires_at", "secret_name", "cookie_path", "session_id").
	// Deliberately absent: hash, header, value, hint, prefix, reset, set,
	// new, old, link, data, which can all carry the credential itself.
	metaWords = set(
		"page", "path", "route", "url", "uri", "endpoint", "file", "dir", "domain", "samesite",
		"name", "id", "ids", "label", "type", "kind", "field", "param", "provider", "source",
		"ttl", "age", "expiry", "expires", "expiration", "expired", "at", "lifetime", "duration",
		"timeout", "interval", "created", "updated", "changed", "rotated",
		"length", "len", "min", "max", "size", "count",
		"policy", "required", "enabled", "strength", "status", "error", "err",
		"fingerprint", "fp", "alg", "algorithm", "version",
	)
)

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// credentialName reports whether name names a credential. wire is true for
// names from a request (query and cookie parameters, header and query maps),
// where a bare "code", "key" or "session" is one.
func credentialName(name string, wire bool) bool {
	words := keyWords(name)
	if len(words) == 1 {
		if always, ok := bareHeads[words[0]]; ok && (wire || always) {
			return true
		}
	}
	last := -1
	for i, w := range words {
		if headWords[w] || (i > 0 && headPairs[w][words[i-1]]) || hasHeadSuffix(w) {
			last = i
		}
	}
	if last < 0 {
		return false
	}
	for _, w := range words[last+1:] {
		if metaWords[w] {
			return false
		}
	}
	return true
}

func hasHeadSuffix(w string) bool {
	for _, h := range headSuffixes {
		if strings.HasSuffix(w, h) {
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

// Patterns RedactString applies, in order, before the JSON field and
// name=value rules.
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
}

var (
	// "name":"value" in JSON; the value is redacted when credentialName(name).
	jsonField = regexp.MustCompile(`"([A-Za-z0-9_.\-]+)"(\s*:\s*)"((?:[^"\\]|\\.)*)"`)
	// The name of a name=value pair: query strings, cookie pairs, "token=abc"
	// in messages. It must start the string or follow one of paramSeps.
	paramName = regexp.MustCompile(`[A-Za-z0-9_.\-]+=`)
)

const (
	paramSeps = "?&;,\"'("  // and whitespace
	paramEnds = "&#;,\"'<>" // and whitespace
)

// RedactString applies the string rules of NewRedactingHandler to s.
func RedactString(s string) string {
	if len(s) < 4 {
		return s
	}
	for _, r := range stringRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	s = jsonField.ReplaceAllStringFunc(s, func(m string) string {
		g := jsonField.FindStringSubmatch(m)
		if !credentialName(g[1], false) || keepValue(g[3]) {
			return m
		}
		return `"` + g[1] + `"` + g[2] + `"` + redacted + `"`
	})
	return redactParams(s)
}

// redactParams replaces the value of every name=value pair whose name is a
// credential (credentialName with wire set). A pair whose name is not one
// leaves its value to be scanned, so "next=/x?token=abc" still loses abc.
func redactParams(s string) string {
	var b strings.Builder
	done := 0 // s[:done] is already in b
	for _, m := range paramName.FindAllStringIndex(s, -1) {
		start, eq := m[0], m[1]
		if start < done || !isParamStart(s[:start]) || !credentialName(s[start:eq-1], true) {
			continue
		}
		end := strings.IndexFunc(s[eq:], func(r rune) bool { return strings.ContainsRune(paramEnds, r) || unicode.IsSpace(r) })
		if end < 0 {
			end = len(s)
		} else {
			end += eq
		}
		if end == eq {
			continue
		}
		b.WriteString(s[done:eq])
		b.WriteString(redacted)
		done = end
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

// isParamStart reports whether a name=value pair may start after before.
func isParamStart(before string) bool {
	if before == "" {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(before)
	return strings.ContainsRune(paramSeps, r) || unicode.IsSpace(r)
}
