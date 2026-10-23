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
	meta, _ := ctx.Value(ctxKey{}).(*requestMeta)
	return meta
}

// meta returns the request data this instance's Middleware loaded, or nil.
// Another instance's data (nested Middleware, e.g. an admin app mounted
// inside the main app) is never used for this instance's sessions or tokens.
func (auth *Auth) meta(ctx context.Context) *requestMeta {
	if meta := metaFrom(ctx); meta != nil && meta.owner == auth {
		return meta
	}
	return nil
}

// userFrom is UserFrom restricted to this instance's session.
func (auth *Auth) userFrom(ctx context.Context) (User, Session, bool) {
	if meta := auth.meta(ctx); meta != nil && meta.ok {
		return meta.user, meta.session, true
	}
	return User{}, Session{}, false
}

// UserFrom returns the signed-in user and session that Middleware loaded
// (the innermost instance's, when several are nested).
func UserFrom(ctx context.Context) (User, Session, bool) {
	if meta := metaFrom(ctx); meta != nil && meta.ok {
		return meta.user, meta.session, true
	}
	return User{}, Session{}, false
}

// cookie returns the full name of one of this instance's cookies:
// "<CookiePrefix>.<name>", with the __Secure- prefix on https (__Host- with
// Config.HostOnlyCookies).
func (auth *Auth) cookie(name string) string {
	full := auth.cfg.CookiePrefix + "." + name
	switch {
	case auth.secure && auth.cfg.HostOnlyCookies:
		return hostPrefix + full
	case auth.secure:
		return securePrefix + full
	}
	return full
}

func (auth *Auth) cookieName() string { return auth.cookie("session_token") }

// setSignedCookie writes an HttpOnly, SameSite=Lax cookie holding a signed
// value; maxAge <= 0 expires it.
func (auth *Auth) setSignedCookie(writer http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(writer, auth.newSignedCookie(name, value, maxAge))
}

// newSignedCookie is the cookie setSignedCookie writes, for callers that
// hand cookies on as data (CompleteSocialSignIn).
func (auth *Auth) newSignedCookie(name, value string, maxAge int) *http.Cookie {
	cookie := &http.Cookie{ //nolint:gosec // Secure follows the BaseURL scheme so http dev servers work
		Name:     name,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   auth.secure,
		SameSite: http.SameSiteLaxMode,
	}
	if maxAge > 0 {
		cookie.Value = encodeCookieValue(auth.signValue(value))
	} else {
		cookie.MaxAge = -1
	}
	return cookie
}

// signedCookie returns the verified value of a signed cookie, or "", and
// whether it was signed with a previous secret (re-sign it).
func (auth *Auth) signedCookie(request *http.Request, name string) (value string, resign bool) {
	cookie, err := request.Cookie(name)
	if err != nil {
		return "", false
	}
	verified, ok, current := auth.verifySignedKey(cookie.Value)
	if !ok {
		return "", false
	}
	return verified, !current
}

// SetSessionCookie writes the signed session cookie for s (HttpOnly,
// SameSite=Lax, Secure on https, expiring with the session).
func (auth *Auth) SetSessionCookie(writer http.ResponseWriter, session Session) {
	http.SetCookie(writer, auth.sessionCookie(session))
}

// sessionCookie is the cookie SetSessionCookie writes for session.
func (auth *Auth) sessionCookie(session Session) *http.Cookie {
	return auth.newSignedCookie(auth.cookieName(), session.Token, int(session.ExpiresAt.Sub(auth.now()).Seconds()))
}

// ClearSessionCookie expires the session cookie.
func (auth *Auth) ClearSessionCookie(writer http.ResponseWriter) {
	auth.setSignedCookie(writer, auth.cookieName(), "", 0)
}

// encodeCookieValue percent-encodes the base64 signature characters like
// encodeURIComponent, which net/http would otherwise reject or quote.
func encodeCookieValue(value string) string {
	return strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(value)
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
func (auth *Auth) sessionToken(request *http.Request) (token string, src tokenSource, resign bool) {
	if header := request.Header.Get("Authorization"); header != "" {
		if token, ok, resign := auth.parseBearer(header); ok {
			return token, fromBearer, resign
		}
	}
	cookie, err := request.Cookie(auth.cookieName())
	if err != nil {
		return "", noToken, false
	}
	token, ok, current := auth.verifySignedKey(cookie.Value)
	if !ok || !isTokenString(token) {
		return "", noToken, false
	}
	return token, fromSessionCookie, !current
}

// clientIP returns the normalised client address: the right-most entry of
// the configured proxy header (the one the trusted proxy appended) when it is
// a valid IP, else the TCP peer.
func (auth *Auth) clientIP(request *http.Request) string {
	if auth.cfg.ClientIPHeader != "" {
		if values := request.Header.Values(auth.cfg.ClientIPHeader); len(values) > 0 {
			parts := strings.Split(values[len(values)-1], ",")
			if ip := normalizeIP(strings.TrimSpace(parts[len(parts)-1])); ip != "" {
				return ip
			}
		}
	}
	host := request.RemoteAddr
	if header, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		host = header
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
func normalizeIP(text string) string {
	addr, err := netip.ParseAddr(text)
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
func (auth *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if auth.meta(request.Context()) != nil {
			next.ServeHTTP(writer, request)
			return
		}
		token, src, resign := auth.sessionToken(request)
		meta := &requestMeta{owner: auth, ip: auth.clientIP(request), userAgent: request.UserAgent(), token: token}
		if auth.cfg.TwoFactor != nil {
			meta.twoFactorToken, _ = auth.signedCookie(request, auth.cookie(twoFactorCookieName)) // lives minutes: no re-sign
			var resignTrust bool
			meta.trustToken, resignTrust = auth.signedCookie(request, auth.cookie(trustDeviceCookieName))
			if resignTrust {
				auth.SetTrustDeviceCookie(writer, meta.trustToken) // the server-side row still bounds its lifetime
			}
		}
		if meta.token != "" {
			user, sess, err := auth.loadSession(request.Context(), meta.token)
			switch {
			case err == nil:
				meta.user, meta.session, meta.ok = user, sess.Session, true
				// A rotated AUTH_SECRET: move the client to the current one
				// while the previous one is still accepted.
				switch {
				case src == fromSessionCookie && resign, sess.refreshed && auth.fromCookie(request):
					auth.SetSessionCookie(writer, sess.Session)
				case src == fromBearer && resign:
					writer.Header().Set("Set-Auth-Token", auth.signValue(meta.token))
					writer.Header().Add("Access-Control-Expose-Headers", "set-auth-token")
				}
			case errors.Is(err, errNotFound):
				if auth.fromCookie(request) {
					auth.ClearSessionCookie(writer)
				}
			default:
				auth.log.ErrorContext(request.Context(), "auth: loading session failed", "err", err)
			}
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), ctxKey{}, meta)))
	})
}

func (auth *Auth) fromCookie(request *http.Request) bool {
	_, err := request.Cookie(auth.cookieName())
	return err == nil && request.Header.Get("Authorization") == ""
}

// RequireUser rejects requests without a session: HTML navigations
// (Accept: text/html) are redirected to Config.LoginPath, everything else gets
// 401 {"data": null, "error": "Unauthorized"}. It applies Middleware itself
// when that has not run yet.
func (auth *Auth) RequireUser(next http.Handler) http.Handler {
	return auth.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, _, ok := auth.userFrom(request.Context()); ok {
			next.ServeHTTP(writer, request)
			return
		}
		if strings.Contains(request.Header.Get("Accept"), "text/html") {
			http.Redirect(writer, request, auth.cfg.LoginPath, http.StatusSeeOther)
			return
		}
		writeJSON(writer, http.StatusUnauthorized, envelope{Data: nil, Error: new("Unauthorized")})
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
func (auth *Auth) loadSession(ctx context.Context, token string) (User, loadedSession, error) {
	sess, user, ban, err := auth.store.sessionWithUser(ctx, token)
	if err != nil {
		return User{}, loadedSession{}, err
	}
	now := auth.now()
	if ban.active(now) {
		// BanUser revokes sessions; this also covers any created meanwhile.
		return User{}, loadedSession{}, errNotFound
	}
	if !sess.ExpiresAt.After(now) {
		if err := auth.store.deleteSession(ctx, token); err != nil {
			auth.log.ErrorContext(ctx, "auth: deleting expired session failed", "err", err)
		}
		return User{}, loadedSession{}, errNotFound
	}
	out := loadedSession{Session: sess}
	if !sess.ExpiresAt.Add(-auth.cfg.SessionTTL).Add(auth.cfg.SessionUpdateAge).After(now) {
		out.ExpiresAt, out.UpdatedAt = now.Add(auth.cfg.SessionTTL), now
		if err := auth.store.extendSession(ctx, sess.ID, out.ExpiresAt, now); err != nil {
			return User{}, loadedSession{}, err
		}
		out.refreshed = true
	}
	return user, out, nil
}

// createSession starts a session for userID, recording the client from ctx.
func (auth *Auth) createSession(ctx context.Context, userID string) (Session, error) {
	if err := auth.checkBan(ctx, userID); err != nil {
		return Session{}, err
	}
	now := auth.now()
	session := Session{
		ID:        generateID(idLength),
		ExpiresAt: now.Add(auth.cfg.SessionTTL),
		Token:     generateID(idLength),
		CreatedAt: now,
		UpdatedAt: now,
		UserID:    userID,
	}
	if meta := auth.meta(ctx); meta != nil {
		if meta.ip != "" {
			session.IPAddress = &meta.ip
		}
		if meta.userAgent != "" {
			session.UserAgent = &meta.userAgent
		}
	}
	return session, auth.store.createSession(ctx, session)
}

// checkBan returns ErrBannedUser while userID is banned, lifting a ban whose
// expiry has passed. Every way of getting a session goes through it.
func (auth *Auth) checkBan(ctx context.Context, userID string) error {
	ban, err := auth.store.banOf(ctx, userID)
	if err != nil {
		return err
	}
	now := auth.now()
	if ban.active(now) {
		return ErrBannedUser
	}
	if ban.banned {
		return auth.store.liftExpiredBan(ctx, userID, now)
	}
	return nil
}
