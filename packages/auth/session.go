package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Cookie names follow Better Auth ("better-auth.session_token", with the
// __Secure- prefix on https) so existing clients keep working.
const (
	sessionCookieName = "better-auth.session_token" // with the default CookiePrefix
	securePrefix      = "__Secure-"
	hostPrefix        = "__Host-" // Config.HostOnlyCookies
)

type ctxKey struct{}

// requestMeta is what Middleware attaches to the request context.
type requestMeta struct {
	ip, userAgent  string
	token          string // session token presented by the client, valid or not
	owner          *Auth  // the instance whose Middleware loaded this
	twoFactorToken string // pending 2FA sign-in (two_factor cookie)
	trustToken     string // "trust this device" (trust_device cookie)
	user           User
	session        Session
	ok             bool
}

func metaFrom(ctx context.Context) *requestMeta {
	m, _ := ctx.Value(ctxKey{}).(*requestMeta)
	return m
}

// meta returns the request data this instance's Middleware loaded, or nil.
// Another instance's data (nested Middleware, e.g. an admin app mounted
// inside the main app) is never used for this instance's sessions or tokens.
func (a *Auth) meta(ctx context.Context) *requestMeta {
	if m := metaFrom(ctx); m != nil && m.owner == a {
		return m
	}
	return nil
}

// userFrom is UserFrom restricted to this instance's session.
func (a *Auth) userFrom(ctx context.Context) (User, Session, bool) {
	if m := a.meta(ctx); m != nil && m.ok {
		return m.user, m.session, true
	}
	return User{}, Session{}, false
}

// UserFrom returns the signed-in user and session that Middleware loaded
// (the innermost instance's, when several are nested).
func UserFrom(ctx context.Context) (User, Session, bool) {
	if m := metaFrom(ctx); m != nil && m.ok {
		return m.user, m.session, true
	}
	return User{}, Session{}, false
}

// cookie returns the full name of one of this instance's cookies:
// "<CookiePrefix>.<name>", with the __Secure- prefix on https (__Host- with
// Config.HostOnlyCookies).
func (a *Auth) cookie(name string) string {
	full := a.cfg.CookiePrefix + "." + name
	switch {
	case a.secure && a.cfg.HostOnlyCookies:
		return hostPrefix + full
	case a.secure:
		return securePrefix + full
	}
	return full
}

func (a *Auth) cookieName() string { return a.cookie("session_token") }

// setSignedCookie writes an HttpOnly, SameSite=Lax cookie holding a signed
// value; maxAge <= 0 expires it.
func (a *Auth) setSignedCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, a.newSignedCookie(name, value, maxAge))
}

// newSignedCookie is the cookie setSignedCookie writes, for callers that
// hand cookies on as data (CompleteSocialSignIn).
func (a *Auth) newSignedCookie(name, value string, maxAge int) *http.Cookie {
	cookie := &http.Cookie{ //nolint:gosec // Secure follows the BaseURL scheme so http dev servers work
		Name:     name,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}
	if maxAge > 0 {
		cookie.Value = encodeCookieValue(a.signValue(value))
	} else {
		cookie.MaxAge = -1
	}
	return cookie
}

// signedCookie returns the verified value of a signed cookie, or "", and
// whether it was signed with a previous secret (re-sign it).
func (a *Auth) signedCookie(r *http.Request, name string) (value string, resign bool) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", false
	}
	v, ok, current := a.verifySignedKey(c.Value)
	if !ok {
		return "", false
	}
	return v, !current
}

// SetSessionCookie writes the signed session cookie for s (HttpOnly,
// SameSite=Lax, Secure on https, expiring with the session).
func (a *Auth) SetSessionCookie(w http.ResponseWriter, s Session) {
	http.SetCookie(w, a.sessionCookie(s))
}

// sessionCookie is the cookie SetSessionCookie writes for session.
func (a *Auth) sessionCookie(session Session) *http.Cookie {
	return a.newSignedCookie(a.cookieName(), session.Token, int(session.ExpiresAt.Sub(a.now()).Seconds()))
}

// ClearSessionCookie expires the session cookie.
func (a *Auth) ClearSessionCookie(w http.ResponseWriter) {
	a.setSignedCookie(w, a.cookieName(), "", 0)
}

// encodeCookieValue percent-encodes the base64 signature characters like
// encodeURIComponent, which net/http would otherwise reject or quote.
func encodeCookieValue(v string) string {
	return strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(v)
}

// tokenSource is where sessionToken found the token.
type tokenSource int

const (
	noToken tokenSource = iota
	fromBearer
	fromSessionCookie
)

// sessionToken returns the token presented by r: a bearer token takes
// precedence over the cookie, as with Better Auth's bearer plugin. resign is
// true when it was signed with a previous secret: re-issue it the same way it
// came (never put a cookie's token in a header).
func (a *Auth) sessionToken(r *http.Request) (token string, src tokenSource, resign bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok, resign := a.parseBearer(h); ok {
			return token, fromBearer, resign
		}
	}
	c, err := r.Cookie(a.cookieName())
	if err != nil {
		return "", noToken, false
	}
	token, ok, current := a.verifySignedKey(c.Value)
	if !ok || !isTokenString(token) {
		return "", noToken, false
	}
	return token, fromSessionCookie, !current
}

// clientIP returns the normalised client address: the right-most entry of
// the configured proxy header (the one the trusted proxy appended) when it is
// a valid IP, else the TCP peer.
func (a *Auth) clientIP(r *http.Request) string {
	if a.cfg.ClientIPHeader != "" {
		if v := r.Header.Values(a.cfg.ClientIPHeader); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			if ip := normalizeIP(strings.TrimSpace(parts[len(parts)-1])); ip != "" {
				return ip
			}
		}
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if ip := normalizeIP(host); ip != "" {
		return ip
	}
	return host
}

const ipv6ClientBits = 64 // prefix length of the /64 one IPv6 client is treated as

// normalizeIP canonicalises an address like Better Auth's normalizeIP:
// IPv4-mapped IPv6 becomes IPv4 and IPv6 is reduced to its /64, since one
// client usually owns a whole /64 and could otherwise rotate past rate limits.
// It returns "" for anything that is not an IP.
func normalizeIP(s string) string {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		return netip.PrefixFrom(addr, ipv6ClientBits).Masked().String()
	}
	return addr.String()
}

// Middleware loads the session from the cookie or "Authorization: Bearer"
// header into the request context (see UserFrom). It never rejects a
// request; wrap handlers with RequireUser for that. Sessions are checked
// against the database on every request, so revocation is immediate, and
// extended once per SessionUpdateAge.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.meta(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		token, src, resign := a.sessionToken(r)
		meta := &requestMeta{owner: a, ip: a.clientIP(r), userAgent: r.UserAgent(), token: token}
		if a.cfg.TwoFactor != nil {
			meta.twoFactorToken, _ = a.signedCookie(r, a.cookie(twoFactorCookieName)) // lives minutes: no re-sign
			var resignTrust bool
			meta.trustToken, resignTrust = a.signedCookie(r, a.cookie(trustDeviceCookieName))
			if resignTrust {
				a.SetTrustDeviceCookie(w, meta.trustToken) // the server-side row still bounds its lifetime
			}
		}
		if meta.token != "" {
			user, sess, err := a.loadSession(r.Context(), meta.token)
			switch {
			case err == nil:
				meta.user, meta.session, meta.ok = user, sess.Session, true
				// A rotated AUTH_SECRET: move the client to the current one
				// while the previous one is still accepted.
				switch {
				case src == fromSessionCookie && resign, sess.refreshed && a.fromCookie(r):
					a.SetSessionCookie(w, sess.Session)
				case src == fromBearer && resign:
					w.Header().Set("Set-Auth-Token", a.signValue(meta.token))
					w.Header().Add("Access-Control-Expose-Headers", "set-auth-token")
				}
			case errors.Is(err, errNotFound):
				if a.fromCookie(r) {
					a.ClearSessionCookie(w)
				}
			default:
				a.log.ErrorContext(r.Context(), "auth: loading session failed", "err", err)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, meta)))
	})
}

func (a *Auth) fromCookie(r *http.Request) bool {
	_, err := r.Cookie(a.cookieName())
	return err == nil && r.Header.Get("Authorization") == ""
}

// RequireUser rejects requests without a session: HTML navigations
// (Accept: text/html) are redirected to Config.LoginPath, everything else gets
// 401 {"data": null, "error": "Unauthorized"}. It applies Middleware itself
// when that has not run yet.
func (a *Auth) RequireUser(next http.Handler) http.Handler {
	return a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := a.userFrom(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, a.cfg.LoginPath, http.StatusSeeOther)
			return
		}
		writeJSON(w, http.StatusUnauthorized, envelope{Data: nil, Error: new("Unauthorized")})
	}))
}

type envelope struct {
	Data  any     `json:"data"`
	Error *string `json:"error"`
}

// loadedSession marks whether loadSession extended the session.
type loadedSession struct {
	Session
	refreshed bool
}

// loadSession returns the live session for token, deleting it when expired
// and extending it when it is older than SessionUpdateAge.
func (a *Auth) loadSession(ctx context.Context, token string) (User, loadedSession, error) {
	sess, user, b, err := a.store.sessionWithUser(ctx, token)
	if err != nil {
		return User{}, loadedSession{}, err
	}
	now := a.now()
	if b.active(now) {
		// BanUser revokes sessions; this also covers any created meanwhile.
		return User{}, loadedSession{}, errNotFound
	}
	if !sess.ExpiresAt.After(now) {
		if err := a.store.deleteSession(ctx, token); err != nil {
			a.log.ErrorContext(ctx, "auth: deleting expired session failed", "err", err)
		}
		return User{}, loadedSession{}, errNotFound
	}
	out := loadedSession{Session: sess}
	if !sess.ExpiresAt.Add(-a.cfg.SessionTTL).Add(a.cfg.SessionUpdateAge).After(now) {
		out.ExpiresAt, out.UpdatedAt = now.Add(a.cfg.SessionTTL), now
		if err := a.store.extendSession(ctx, sess.ID, out.ExpiresAt, now); err != nil {
			return User{}, loadedSession{}, err
		}
		out.refreshed = true
	}
	return user, out, nil
}

// createSession starts a session for userID, recording the client from ctx.
func (a *Auth) createSession(ctx context.Context, userID string) (Session, error) {
	if err := a.checkBan(ctx, userID); err != nil {
		return Session{}, err
	}
	now := a.now()
	s := Session{
		ID:        generateID(idLength),
		ExpiresAt: now.Add(a.cfg.SessionTTL),
		Token:     generateID(idLength),
		CreatedAt: now,
		UpdatedAt: now,
		UserID:    userID,
	}
	if m := a.meta(ctx); m != nil {
		if m.ip != "" {
			s.IPAddress = &m.ip
		}
		if m.userAgent != "" {
			s.UserAgent = &m.userAgent
		}
	}
	return s, a.store.createSession(ctx, s)
}

// checkBan returns ErrBannedUser while userID is banned, lifting a ban whose
// expiry has passed. Every way of getting a session goes through it.
func (a *Auth) checkBan(ctx context.Context, userID string) error {
	b, err := a.store.banOf(ctx, userID)
	if err != nil {
		return err
	}
	now := a.now()
	if b.active(now) {
		return ErrBannedUser
	}
	if b.banned {
		return a.store.liftExpiredBan(ctx, userID, now)
	}
	return nil
}
