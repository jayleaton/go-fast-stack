package auth

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// originOf returns scheme://host[:port] with the scheme and host lowercased
// and default ports dropped, like the WHATWG URL origin.
func originOf(link *url.URL) string {
	scheme := strings.ToLower(link.Scheme)
	host := strings.ToLower(link.Hostname())
	port := link.Port()
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

func hasControlChar(text string) bool {
	for _, char := range text {
		if char < 0x20 || (char >= 0x7f && char <= 0x9f) {
			return true
		}
	}
	return false
}

// isSafeRelativeURL accepts root-relative paths that every browser and router
// resolves to the same origin ("/a?b#c"), rejecting "//evil", "/\evil" and
// encoded separators.
func isSafeRelativeURL(value string) bool {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.Contains(value, "\\") || hasControlChar(value) {
		return false
	}
	path := value
	if index := strings.IndexAny(value, "?#"); index >= 0 {
		path = value[:index]
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return false
	}
	link, err := url.Parse(value)
	return err == nil && link.Scheme == "" && link.Host == "" && link.User == nil
}

// customScheme splits a non-http(s) origin such as "betterstack://" or
// "exp://192.168.1.2:8081/--/path" into lowercased scheme and authority and a
// cleaned path, without url.Parse, so traversal cannot escape a pinned path.
func customScheme(value string) (scheme, authority, path string, ok bool) {
	if hasControlChar(value) {
		return "", "", "", false
	}
	index := strings.IndexByte(value, ':')
	if index <= 0 {
		return "", "", "", false
	}
	scheme, rest := strings.ToLower(value[:index]), value[index+1:]
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
	for key := range strings.SplitSeq(rest, "/") {
		switch key {
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		case ".", "":
		default:
			segs = append(segs, key)
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
	var builder strings.Builder
	builder.WriteString("^")
	for index := 0; index < len(pattern); index++ {
		switch char := pattern[index]; {
		case char == '*' && index+1 < len(pattern) && pattern[index+1] == '*':
			builder.WriteString(".*")
			index++
		case char == '*':
			builder.WriteString(`[^/\\]*`)
		case char == '?':
			builder.WriteString(`[^/\\]`)
		default:
			builder.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	builder.WriteString("$")
	return regexp.MustCompile(builder.String())
}

// matchesOrigin ports Better Auth's matchesOriginPattern: v is a full URL (or
// a relative path when allowRelative), pattern an origin or wildcard pattern.
func matchesOrigin(value, pattern string, allowRelative bool) bool {
	if strings.HasPrefix(value, "/") {
		return allowRelative && isSafeRelativeURL(value)
	}
	if hasControlChar(value) {
		return false
	}
	if strings.ContainsAny(pattern, "*?") {
		re := globRegexp(pattern)
		if strings.Contains(pattern, "://") {
			if link, err := url.Parse(value); err == nil && (link.Scheme == "http" || link.Scheme == "https") && link.Host != "" {
				return re.MatchString(originOf(link))
			}
			return re.MatchString(value)
		}
		// Host-only patterns are for web origins: refuse javascript: and
		// other schemes that happen to have a matching host.
		link, err := url.Parse(value)
		if err != nil || link.Host == "" || (link.Scheme != "http" && link.Scheme != "https") {
			return false
		}
		return re.MatchString(strings.ToLower(link.Host))
	}
	scheme, _, _, ok := customScheme(value)
	if !ok || scheme == "http" || scheme == "https" {
		link, err := url.Parse(value)
		if err != nil || link.Host == "" {
			return false
		}
		return originOf(link) == strings.TrimRight(pattern, "/")
	}
	ps, pa, pp, pok := customScheme(pattern)
	_, va, vp, _ := customScheme(value)
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
func (auth *Auth) isTrusted(value string, allowRelative bool) bool {
	for _, path := range auth.trusted {
		if matchesOrigin(value, path, allowRelative) {
			return true
		}
	}
	return false
}

// checkCallbackURL validates a client-supplied redirect target (open-redirect
// protection). Empty is allowed.
// CheckCallbackURL reports whether v may be redirected to after an emailed
// link: a safe relative path, or a URL on a trusted origin or deep-link
// scheme (Config.TrustedOrigins). It returns ErrInvalidCallbackURL otherwise.
func (auth *Auth) CheckCallbackURL(value string) error {
	if value == "" {
		return ErrInvalidCallbackURL
	}
	return auth.checkCallbackURL(value, ErrInvalidCallbackURL)
}

func (auth *Auth) checkCallbackURL(value string, errValue *Error) error {
	if value == "" || auth.isTrusted(value, true) {
		return nil
	}
	auth.log.Warn("auth: untrusted redirect target", "url", value)
	return errValue
}

// requestOrigin returns the caller's origin: Origin, else Expo's expo-origin
// (native apps cannot set Origin), else Referer.
func requestOrigin(request *http.Request) string {
	if origin := request.Header.Get("Origin"); origin != "" {
		return origin
	}
	if origin := request.Header.Get("Expo-Origin"); origin != "" {
		return origin
	}
	return request.Header.Get("Referer")
}

// validateOrigin is Better Auth's CSRF origin check. Requests carrying
// cookies (or any request when force is set) must come from a trusted origin.
// Cookie-less requests (bearer clients, native apps) cannot be forged by a
// browser and pass.
func (auth *Auth) validateOrigin(request *http.Request, force bool) error {
	if !force && request.Header.Get("Cookie") == "" {
		return nil
	}
	origin := requestOrigin(request)
	if origin == "null" && request.Header.Get("Sec-Fetch-Site") == "same-origin" {
		// Privacy contexts send "null" for same-origin requests; Better Auth
		// then trusts the browser's same-origin verdict.
		return nil
	}
	if origin == "" || origin == "null" {
		return ErrMissingOrigin
	}
	if !auth.isTrusted(origin, false) {
		auth.log.Warn("auth: request from untrusted origin", "origin", origin)
		return ErrInvalidOrigin
	}
	return nil
}

// checkFormCSRF guards sign-in and sign-up, which a victim has no cookie for
// yet: with cookies it is the origin check; without, browsers identify
// themselves via Fetch Metadata, so cross-site navigations are refused and any
// other browser request must have a trusted origin.
func (auth *Auth) checkFormCSRF(request *http.Request) error {
	if request.Header.Get("Cookie") != "" {
		return auth.validateOrigin(request, false)
	}
	site := strings.TrimSpace(request.Header.Get("Sec-Fetch-Site"))
	mode := strings.TrimSpace(request.Header.Get("Sec-Fetch-Mode"))
	dest := strings.TrimSpace(request.Header.Get("Sec-Fetch-Dest"))
	if site == "" && mode == "" && dest == "" {
		return nil
	}
	if site == "cross-site" && mode == "navigate" {
		return ErrCrossSiteNavigation
	}
	return auth.validateOrigin(request, true)
}
