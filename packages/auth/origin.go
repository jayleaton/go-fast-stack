package auth

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// originOf returns scheme://host[:port] with the scheme and host lowercased
// and default ports dropped, like the WHATWG URL origin.
func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

// isSafeRelativeURL accepts root-relative paths that every browser and router
// resolves to the same origin ("/a?b#c"), rejecting "//evil", "/\evil" and
// encoded separators.
func isSafeRelativeURL(v string) bool {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.Contains(v, "\\") || hasControlChar(v) {
		return false
	}
	path := v
	if i := strings.IndexAny(v, "?#"); i >= 0 {
		path = v[:i]
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "" && u.Host == "" && u.User == nil
}

// customScheme splits a non-http(s) origin such as "betterstack://" or
// "exp://192.168.1.2:8081/--/path" into lowercased scheme and authority and a
// cleaned path, without url.Parse, so traversal cannot escape a pinned path.
func customScheme(v string) (scheme, authority, path string, ok bool) {
	if hasControlChar(v) {
		return "", "", "", false
	}
	i := strings.IndexByte(v, ':')
	if i <= 0 {
		return "", "", "", false
	}
	scheme, rest := strings.ToLower(v[:i]), v[i+1:]
	if after, found := strings.CutPrefix(rest, "//"); found {
		rest = after
		end := strings.IndexAny(rest, "/?#")
		if end == -1 {
			end = len(rest)
		}
		authority, rest = strings.ToLower(rest[:end]), rest[end:]
	}
	if end := strings.IndexAny(rest, "?#"); end >= 0 {
		rest = rest[:end]
	}
	if decoded, err := url.PathUnescape(rest); err == nil {
		rest = decoded
	}
	var segs []string
	for s := range strings.SplitSeq(rest, "/") {
		switch s {
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		case ".", "":
		default:
			segs = append(segs, s)
		}
	}
	if len(segs) > 0 {
		path = "/" + strings.Join(segs, "/")
	}
	return scheme, authority, path, true
}

// globRegexp compiles a Better Auth origin pattern: "*" and "?" match within
// one path segment, "**" matches anything.
func globRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString(`[^/\\]*`)
		case c == '?':
			b.WriteString(`[^/\\]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// matchesOrigin ports Better Auth's matchesOriginPattern: v is a full URL (or
// a relative path when allowRelative), pattern an origin or wildcard pattern.
func matchesOrigin(v, pattern string, allowRelative bool) bool {
	if strings.HasPrefix(v, "/") {
		return allowRelative && isSafeRelativeURL(v)
	}
	if hasControlChar(v) {
		return false
	}
	if strings.ContainsAny(pattern, "*?") {
		re := globRegexp(pattern)
		if strings.Contains(pattern, "://") {
			if u, err := url.Parse(v); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
				return re.MatchString(originOf(u))
			}
			return re.MatchString(v)
		}
		// Host-only patterns are for web origins: refuse javascript: and
		// other schemes that happen to have a matching host.
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
		return re.MatchString(strings.ToLower(u.Host))
	}
	scheme, _, _, ok := customScheme(v)
	if !ok || scheme == "http" || scheme == "https" {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return false
		}
		return originOf(u) == strings.TrimRight(pattern, "/")
	}
	ps, pa, pp, pok := customScheme(pattern)
	_, va, vp, _ := customScheme(v)
	if !pok || ps != scheme {
		return false
	}
	if pa != "" && pa != va {
		return false
	}
	return pp == "" || vp == pp || strings.HasPrefix(vp, pp+"/")
}

// isTrusted reports whether v (a URL, or a relative path when allowRelative)
// belongs to the base URL or a trusted origin.
func (a *Auth) isTrusted(v string, allowRelative bool) bool {
	for _, p := range a.trusted {
		if matchesOrigin(v, p, allowRelative) {
			return true
		}
	}
	return false
}

// checkCallbackURL validates a client-supplied redirect target (open-redirect
// protection). Empty is allowed.
func (a *Auth) checkCallbackURL(v string, errValue *Error) error {
	if v == "" || a.isTrusted(v, true) {
		return nil
	}
	a.log.Warn("auth: untrusted redirect target", "url", v)
	return errValue
}

// requestOrigin returns the caller's origin: Origin, else Expo's expo-origin
// (native apps cannot set Origin), else Referer.
func requestOrigin(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" {
		return o
	}
	if o := r.Header.Get("Expo-Origin"); o != "" {
		return o
	}
	return r.Header.Get("Referer")
}

// validateOrigin is Better Auth's CSRF origin check. Requests carrying
// cookies (or any request when force is set) must come from a trusted origin.
// Cookie-less requests (bearer clients, native apps) cannot be forged by a
// browser and pass.
func (a *Auth) validateOrigin(r *http.Request, force bool) error {
	if !force && r.Header.Get("Cookie") == "" {
		return nil
	}
	origin := requestOrigin(r)
	if origin == "null" && r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		// Privacy contexts send "null" for same-origin requests; Better Auth
		// then trusts the browser's same-origin verdict.
		return nil
	}
	if origin == "" || origin == "null" {
		return ErrMissingOrigin
	}
	if !a.isTrusted(origin, false) {
		a.log.Warn("auth: request from untrusted origin", "origin", origin)
		return ErrInvalidOrigin
	}
	return nil
}

// checkFormCSRF guards sign-in and sign-up, which a victim has no cookie for
// yet: with cookies it is the origin check; without, browsers identify
// themselves via Fetch Metadata, so cross-site navigations are refused and any
// other browser request must have a trusted origin.
func (a *Auth) checkFormCSRF(r *http.Request) error {
	if r.Header.Get("Cookie") != "" {
		return a.validateOrigin(r, false)
	}
	site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
	mode := strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode"))
	dest := strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest"))
	if site == "" && mode == "" && dest == "" {
		return nil
	}
	if site == "cross-site" && mode == "navigate" {
		return ErrCrossSiteNavigation
	}
	return a.validateOrigin(r, true)
}
