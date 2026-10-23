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
func NewRedactingHandler(handler slog.Handler, opts RedactOptions) slog.Handler {
	return &redactingHandler{next: handler, opts: opts}
}

type redactingHandler struct {
	next slog.Handler
	opts RedactOptions
}

func (handler *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	keepURL := handler.opts.KeepDevMailLinks && isDevMailMessage(record.Message)
	out := slog.NewRecord(record.Time, record.Level, RedactString(record.Message), record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		if keepURL && attribute.Key == devMailLinkKey {
			out.AddAttrs(attribute)
		} else {
			out.AddAttrs(redactAttr(attribute, false))
		}
		return true
	})
	return handler.next.Handle(ctx, out)
}

func (handler *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for index, attribute := range attrs {
		red[index] = redactAttr(attribute, false)
	}
	return &redactingHandler{next: handler.next.WithAttrs(red), opts: handler.opts}
}

func (handler *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: handler.next.WithGroup(name), opts: handler.opts}
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
func redactAttr(attribute slog.Attr, wire bool) slog.Attr {
	value := attribute.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		if credentialName(attribute.Key, wire) {
			return slog.String(attribute.Key, redacted)
		}
		attrs := value.Group()
		red := make([]slog.Attr, len(attrs))
		for index, groupAttr := range attrs {
			red[index] = redactAttr(groupAttr, wire)
		}
		return slog.Attr{Key: attribute.Key, Value: slog.GroupValue(red...)}
	}
	if credentialName(attribute.Key, wire) {
		if value.Kind() == slog.KindBool || (value.Kind() == slog.KindString && keepValue(value.String())) {
			return slog.Attr{Key: attribute.Key, Value: value} // has_password=true carries no credential
		}
		return slog.String(attribute.Key, redacted)
	}
	switch value.Kind() {
	case slog.KindString:
		return slog.String(attribute.Key, RedactString(value.String()))
	case slog.KindAny:
		if group, ok := mapGroup(value.Any()); ok {
			entries := group.Group()
			for index, entry := range entries {
				entries[index] = redactAttr(entry, true)
			}
			return slog.Attr{Key: attribute.Key, Value: slog.GroupValue(entries...)}
		}
		text, ok := textOf(value.Any())
		if !ok {
			text, ok = jsonOf(value.Any())
		}
		if ok {
			if red := RedactString(text); red != text {
				return slog.String(attribute.Key, red)
			}
		}
	}
	return slog.Attr{Key: attribute.Key, Value: value}
}

// mapGroup turns a map with string keys (http.Header, url.Values,
// map[string]string) into a group, so each entry is redacted by its key,
// read like a request parameter name (a bare "code" or "key" counts).
// Single-value slices log as their value.
func mapGroup(value any) (slog.Value, bool) {
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return slog.Value{}, false
	}
	keys := rv.MapKeys()
	slices.SortFunc(keys, func(left, right reflect.Value) int { return strings.Compare(left.String(), right.String()) })
	attrs := make([]slog.Attr, 0, len(keys))
	for _, key := range keys {
		element := rv.MapIndex(key)
		if element.Kind() == reflect.Slice && element.Len() == 1 {
			element = element.Index(0)
		}
		attrs = append(attrs, slog.Any(key.String(), element.Interface()))
	}
	return slog.GroupValue(attrs...), true
}

// jsonOf is the JSON a handler would print for a struct (or pointer to
// one), so credential fields inside it ("token":"…") are seen.
func jsonOf(value any) (string, bool) {
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return "", false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// textOf returns the text a handler would print for errors, Stringers and
// string slices; ok is false for anything else.
func textOf(value any) (text string, ok bool) {
	defer func() {
		if recover() != nil { // a nil-pointer String(): leave it to the handler
			text, ok = "", false
		}
	}()
	switch typed := value.(type) {
	case error:
		return typed.Error(), true
	case fmt.Stringer:
		return typed.String(), true
	case []string:
		return fmt.Sprint(typed), true
	}
	return "", false
}

func isFingerprint(text string) bool {
	if text == "none" {
		return true
	}
	header, ok := strings.CutPrefix(text, "fp:")
	return ok && len(header) == 8 && strings.Trim(header, "0123456789abcdef") == ""
}

// keepValue reports whether a value under a credential name may stay
// readable: it is empty, already redacted, or a Fingerprint().
func keepValue(text string) bool {
	return text == "" || text == redacted || isFingerprint(text)
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
	lookup := make(map[string]bool, len(words))
	for _, word := range words {
		lookup[word] = true
	}
	return lookup
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
	for index, word := range words {
		if headWords[word] || (index > 0 && headPairs[word][words[index-1]]) || hasHeadSuffix(word) {
			last = index
		}
	}
	if last < 0 {
		return false
	}
	for _, word := range words[last+1:] {
		if metaWords[word] {
			return false
		}
	}
	return true
}

func hasHeadSuffix(word string) bool {
	for _, headSuffix := range headSuffixes {
		if strings.HasSuffix(word, headSuffix) {
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
	for index, char := range rs {
		switch {
		case char == '-' || char == '_' || char == '.' || unicode.IsSpace(char):
			flush()
			continue
		case unicode.IsUpper(char) && index > 0 && (unicode.IsLower(rs[index-1]) ||
			(unicode.IsUpper(rs[index-1]) && index+1 < len(rs) && unicode.IsLower(rs[index+1]))):
			flush() // fooBar → foo|Bar, HTTPHeader → HTTP|Header
		}
		cur = append(cur, char)
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

// minRedactLength is a quick exit: a string shorter than this cannot hold a
// credential worth redacting.
const minRedactLength = 4

const (
	paramSeps = "?&;,\"'("  // and whitespace
	paramEnds = "&#;,\"'<>" // and whitespace
)

// RedactString applies the string rules of NewRedactingHandler to s.
func RedactString(text string) string {
	if len(text) < minRedactLength {
		return text
	}
	for _, stringRule := range stringRules {
		text = stringRule.re.ReplaceAllString(text, stringRule.repl)
	}
	text = jsonField.ReplaceAllStringFunc(text, func(message string) string {
		submatch := jsonField.FindStringSubmatch(message)
		if !credentialName(submatch[1], false) || keepValue(submatch[3]) {
			return message
		}
		return `"` + submatch[1] + `"` + submatch[2] + `"` + redacted + `"`
	})
	return redactParams(text)
}

// redactParams replaces the value of every name=value pair whose name is a
// credential (credentialName with wire set). A pair whose name is not one
// leaves its value to be scanned, so "next=/x?token=abc" still loses abc.
func redactParams(text string) string {
	var builder strings.Builder
	done := 0 // s[:done] is already in b
	for _, counts := range paramName.FindAllStringIndex(text, -1) {
		start, eq := counts[0], counts[1]
		if start < done || !isParamStart(text[:start]) || !credentialName(text[start:eq-1], true) {
			continue
		}
		end := strings.IndexFunc(text[eq:], func(char rune) bool { return strings.ContainsRune(paramEnds, char) || unicode.IsSpace(char) })
		if end < 0 {
			end = len(text)
		} else {
			end += eq
		}
		if end == eq {
			continue
		}
		builder.WriteString(text[done:eq])
		builder.WriteString(redacted)
		done = end
	}
	if done == 0 {
		return text
	}
	builder.WriteString(text[done:])
	return builder.String()
}

// isParamStart reports whether a name=value pair may start after before.
func isParamStart(before string) bool {
	if before == "" {
		return true
	}
	char, _ := utf8.DecodeLastRuneInString(before)
	return strings.ContainsRune(paramSeps, char) || unicode.IsSpace(char)
}
