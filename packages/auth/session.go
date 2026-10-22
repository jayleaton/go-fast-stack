package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
)

// Cookie names follow Better Auth ("better-auth.session_token", with the
// __Secure- prefix on https) so existing clients keep working.
const (
	sessionCookieName = "better-auth.session_token"
	securePrefix      = "__Secure-"
)

type ctxKey struct{}

// requestMeta is what Middleware attaches to the request context.
type requestMeta struct {
	ip, userAgent string
	token         string // session token presented by the client, valid or not
	user          User
	session       Session
	ok            bool
}

func metaFrom(ctx context.Context) *requestMeta {
	m, _ := ctx.Value(ctxKey{}).(*requestMeta)
	return m
}

// UserFrom returns the signed-in user and session that Middleware loaded.
func UserFrom(ctx context.Context) (User, Session, bool) {
	if m := metaFrom(ctx); m != nil && m.ok {
		return m.user, m.session, true
	}
	return User{}, Session{}, false
}

func (a *Auth) cookieName() string {
	if a.secure {
		return securePrefix + sessionCookieName
	}
	return sessionCookieName
}

// SetSessionCookie writes the signed session cookie for s (HttpOnly,
// SameSite=Lax, Secure on https, expiring with the session).
func (a *Auth) SetSessionCookie(w http.ResponseWriter, s Session) {
	maxAge := int(s.ExpiresAt.Sub(a.now()).Seconds())
	if maxAge <= 0 {
		a.ClearSessionCookie(w)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the BaseURL scheme so http dev servers work
		Name:     a.cookieName(),
		Value:    encodeCookieValue(a.signValue(s.Token)),
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the session cookie.
func (a *Auth) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the BaseURL scheme so http dev servers work
		Name:     a.cookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// encodeCookieValue percent-encodes the base64 signature characters like
// encodeURIComponent, which net/http would otherwise reject or quote.
func encodeCookieValue(v string) string {
	return strings.NewReplacer("+", "%2B", "/", "%2F", "=", "%3D").Replace(v)
}

// sessionToken returns the token presented by r: a bearer token takes
// precedence over the cookie, as with Better Auth's bearer plugin.
func (a *Auth) sessionToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok := a.parseBearer(h); ok {
			return token
		}
	}
	c, err := r.Cookie(a.cookieName())
	if err != nil {
		return ""
	}
	token, ok := a.verifySigned(c.Value)
	if !ok || !isTokenString(token) {
		return ""
	}
	return token
}

// clientIP returns the client address: the right-most entry of the
// configured proxy header (the one the trusted proxy appended), else the
// TCP peer.
func (a *Auth) clientIP(r *http.Request) string {
	if a.cfg.ClientIPHeader != "" {
		if v := r.Header.Values(a.cfg.ClientIPHeader); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// Middleware loads the session from the cookie or "Authorization: Bearer"
// header into the request context (see UserFrom). It never rejects a
// request; wrap handlers with RequireUser for that. Sessions are checked
// against the database on every request, so revocation is immediate, and
// extended once per SessionUpdateAge.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if metaFrom(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		meta := &requestMeta{ip: a.clientIP(r), userAgent: r.UserAgent(), token: a.sessionToken(r)}
		if meta.token != "" {
			user, sess, err := a.loadSession(r.Context(), meta.token)
			switch {
			case err == nil:
				meta.user, meta.session, meta.ok = user, sess.Session, true
				if sess.refreshed && a.fromCookie(r) {
					a.SetSessionCookie(w, sess.Session)
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
		if _, _, ok := UserFrom(r.Context()); ok {
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
	sess, user, err := a.store.sessionWithUser(ctx, token)
	if err != nil {
		return User{}, loadedSession{}, err
	}
	now := a.now()
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
	now := a.now()
	s := Session{
		ID:        generateID(32),
		ExpiresAt: now.Add(a.cfg.SessionTTL),
		Token:     generateID(32),
		CreatedAt: now,
		UpdatedAt: now,
		UserID:    userID,
	}
	if m := metaFrom(ctx); m != nil {
		if m.ip != "" {
			s.IPAddress = &m.ip
		}
		if m.userAgent != "" {
			s.UserAgent = &m.userAgent
		}
	}
	return s, a.store.createSession(ctx, s)
}
