// The preview's in-memory backend: ui.Auth and ui.Users with packages/auth's
// and services.UserService's rules, close enough to click through every page.
// Sessions live in a cookie, tokens in maps, and "emails" are logged.
// Tests use the generated mocks in apps/web/ui/mocks instead.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
	"github.com/jayleaton/go-fast-stack/apps/web/ui"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// CookieName matches Better Auth's session cookie.
const CookieName = "better-auth.session_token"

// SessionTTL matches the auth contract default.
const SessionTTL = 7 * 24 * time.Hour

// Mail is one email the fake would have sent.
type Mail struct {
	Kind string // "verify", "reset" or "change-email"
	To   string
	URL  string
}

type account struct {
	user     ui.User
	password string
}

// Store is the shared state behind Auth and Users.
type Store struct {
	// RequireEmailVerification blocks sign-in until the email is verified.
	RequireEmailVerification bool
	// OnMail is called for every outgoing email (the preview logs the link).
	OnMail func(Mail)
	Now    func() time.Time

	mu       sync.Mutex
	accounts map[string]*account // by user ID
	sessions map[string]ui.Session
	verify   map[string]string // token -> user ID
	reset    map[string]string // token -> user ID
	// emailChanges are pending change-email links by token.
	emailChanges map[string]emailChange
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		Now:          time.Now,
		accounts:     map[string]*account{},
		sessions:     map[string]ui.Session{},
		verify:       map[string]string{},
		reset:        map[string]string{},
		emailChanges: map[string]emailChange{},
	}
}

// AddUser creates an account directly (seed data).
func (s *Store) AddUser(name, email, password string, verified bool) ui.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := ui.User{ID: token(), Name: name, Email: email, EmailVerified: verified, CreatedAt: s.Now()}
	s.accounts[u.ID] = &account{user: u, password: password}
	return u
}

// NewSession signs a user in directly (tests).
func (s *Store) NewSession(userID, userAgent, ip string, age time.Duration) ui.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.WithValue(context.Background(), metaKey{}, requestMeta{userAgent, ip})
	sess := s.newSessionLocked(ctx, userID)
	sess.CreatedAt = sess.CreatedAt.Add(-age)
	s.sessions[sess.Token] = sess
	return sess
}

func (s *Store) newSessionLocked(ctx context.Context, userID string) ui.Session {
	now := s.Now()
	sess := ui.Session{ID: token(), Token: token(), UserID: userID, CreatedAt: now, ExpiresAt: now.Add(SessionTTL)}
	if m, ok := ctx.Value(metaKey{}).(requestMeta); ok {
		sess.UserAgent, sess.IPAddress = &m.userAgent, &m.ip
	}
	s.sessions[sess.Token] = sess
	return sess
}

type metaKey struct{}

type requestMeta struct{ userAgent, ip string }

func (s *Store) byEmailLocked(email string) *account {
	for _, a := range s.accounts {
		if strings.EqualFold(a.user.Email, email) {
			return a
		}
	}
	return nil
}

func (s *Store) mailLocked(kind, to, path string, tok string) {
	m := Mail{Kind: kind, To: to, URL: path + "?" + url.Values{"token": {tok}}.Encode()}
	if s.OnMail != nil {
		s.OnMail(m)
	}
}

func token() string { return rand.Text() }

// Auth implements ui.Auth.
type Auth struct{ S *Store }

var _ ui.Auth = Auth{}

func (a Auth) SignUpEmail(ctx context.Context, in ui.SignUpInput) (ui.User, ui.Session, error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(in.Email, "@") {
		return ui.User{}, ui.Session{}, auth.ErrInvalidEmail
	}
	if err := checkPassword(in.Password); err != nil {
		return ui.User{}, ui.Session{}, err
	}
	if existing := s.byEmailLocked(in.Email); existing != nil {
		// Like auth: with verification on, a taken email looks like success.
		if s.RequireEmailVerification {
			return ui.User{Name: in.Name, Email: in.Email}, ui.Session{}, nil
		}
		return ui.User{}, ui.Session{}, auth.ErrUserAlreadyExists
	}
	u := ui.User{ID: token(), Name: in.Name, Email: in.Email, CreatedAt: s.Now()}
	s.accounts[u.ID] = &account{user: u, password: in.Password}
	if s.RequireEmailVerification {
		tok := token()
		s.verify[tok] = u.ID
		s.mailLocked("verify", u.Email, in.CallbackURL, tok)
		return u, ui.Session{}, nil
	}
	return u, s.newSessionLocked(ctx, u.ID), nil
}

func (a Auth) SignInEmail(ctx context.Context, in ui.SignInInput) (ui.User, ui.Session, error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.byEmailLocked(in.Email)
	if acc == nil || acc.password != in.Password {
		return ui.User{}, ui.Session{}, auth.ErrInvalidEmailOrPassword
	}
	if s.RequireEmailVerification && !acc.user.EmailVerified {
		return ui.User{}, ui.Session{}, auth.ErrEmailNotVerified
	}
	return acc.user, s.newSessionLocked(ctx, acc.user.ID), nil
}

func (a Auth) SignOut(_ context.Context, sessionToken string) error {
	a.S.mu.Lock()
	defer a.S.mu.Unlock()
	delete(a.S.sessions, sessionToken)
	return nil
}

func (a Auth) RequestPasswordReset(_ context.Context, email, redirectTo string) error {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	// Unknown emails succeed silently, so the form does not reveal accounts.
	if acc := s.byEmailLocked(email); acc != nil {
		tok := token()
		s.reset[tok] = acc.user.ID
		s.mailLocked("reset", email, redirectTo, tok)
	}
	return nil
}

func (a Auth) ResetPassword(_ context.Context, tok, newPassword string) error {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.reset[tok]
	if !ok || s.accounts[id] == nil {
		return auth.ErrInvalidToken
	}
	if err := checkPassword(newPassword); err != nil {
		return err
	}
	delete(s.reset, tok)
	s.accounts[id].password = newPassword
	return nil
}

func (a Auth) VerifyEmail(ctx context.Context, tok string) (ui.User, ui.Session, error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.verify[tok]
	if !ok || s.accounts[id] == nil {
		return ui.User{}, ui.Session{}, auth.ErrInvalidToken
	}
	delete(s.verify, tok)
	acc := s.accounts[id]
	if acc.user.EmailVerified {
		return acc.user, ui.Session{}, nil
	}
	acc.user.EmailVerified = true
	return acc.user, s.newSessionLocked(ctx, id), nil
}

// CheckCallbackURL trusts this site's paths only: the preview has no mobile app.
func (Auth) CheckCallbackURL(callbackURL string) error {
	if strings.HasPrefix(callbackURL, "/") && !strings.HasPrefix(callbackURL, "//") {
		return nil
	}
	return errors.New("untrusted callback")
}

func (a Auth) SendVerificationEmail(_ context.Context, email, callbackURL string) error {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc := s.byEmailLocked(email); acc != nil && !acc.user.EmailVerified {
		tok := token()
		s.verify[tok] = acc.user.ID
		s.mailLocked("verify", email, callbackURL, tok)
	}
	return nil
}

func (a Auth) SetSessionCookie(w http.ResponseWriter, sess ui.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sess.Token,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a Auth) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

type sessionKey struct{}

type current struct {
	user    ui.User
	session ui.Session
}

// UserFrom is the preview's ui.Deps.UserFrom.
func (a Auth) UserFrom(ctx context.Context) (ui.User, ui.Session, bool) {
	c, ok := ctx.Value(sessionKey{}).(current)
	return c.user, c.session, ok
}

// Middleware loads the session from the cookie, like auth.Middleware.
func (a Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := strings.Cut(r.RemoteAddr, ":")
		r = r.WithContext(context.WithValue(r.Context(), metaKey{}, requestMeta{r.UserAgent(), ip}))
		if c, err := r.Cookie(CookieName); err == nil {
			s := a.S
			s.mu.Lock()
			sess, ok := s.sessions[c.Value]
			var acc *account
			if ok && sess.ExpiresAt.After(s.Now()) {
				acc = s.accounts[sess.UserID]
			}
			var cur current
			if acc != nil {
				cur = current{user: acc.user, session: sess}
			}
			s.mu.Unlock()
			if acc != nil {
				r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, cur))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Users implements ui.Users with services.UserService's rules.
type Users struct{ S *Store }

var _ ui.Users = Users{}

// own locks the store and returns the actor's account, or the service error.
func (u Users) own(actor services.Actor, userID string) (*account, func(), error) {
	if actor.UserID == "" || actor.UserID != userID {
		return nil, nil, services.ErrForbidden
	}
	u.S.mu.Lock()
	acc := u.S.accounts[userID]
	if acc == nil {
		u.S.mu.Unlock()
		return nil, nil, services.ErrUserNotFound
	}
	return acc, u.S.mu.Unlock, nil
}

func toService(a *account) services.User {
	x := a.user
	return services.User{ID: x.ID, Name: x.Name, Email: x.Email, EmailVerified: x.EmailVerified, Image: x.Image, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}
}

func (u Users) GetProfile(_ context.Context, actor services.Actor, userID string) (services.User, error) {
	acc, unlock, err := u.own(actor, userID)
	if err != nil {
		return services.User{}, err
	}
	defer unlock()
	return toService(acc), nil
}

func (u Users) UpdateProfile(_ context.Context, actor services.Actor, userID string, in validate.UpdateUserSettings) (services.User, error) {
	acc, unlock, err := u.own(actor, userID)
	if err != nil {
		return services.User{}, err
	}
	defer unlock()
	if in.Name != nil {
		acc.user.Name = *in.Name
	}
	if in.Image.Set {
		acc.user.Image = in.Image.Value
	}
	acc.user.UpdatedAt = u.S.Now()
	return toService(acc), nil
}

func (u Users) RequestEmailChange(_ context.Context, actor services.Actor, userID string, in validate.UpdateUserEmail) error {
	acc, unlock, err := u.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	if strings.EqualFold(in.NewEmail, acc.user.Email) {
		return services.ErrSameEmail
	}
	if other := u.S.byEmailLocked(in.NewEmail); other != nil {
		return services.ErrEmailTaken
	}
	tok := token()
	u.S.emailChanges[tok] = emailChange{userID: userID, email: in.NewEmail, callback: in.CallbackURL}
	u.S.mailLocked("change-email", in.NewEmail, confirmEmailPath, tok)
	return nil
}

func (u Users) ChangePassword(_ context.Context, actor services.Actor, userID string, in validate.UpdateUserPassword) error {
	acc, unlock, err := u.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	if checkPassword(in.NewPassword) != nil {
		return services.ErrPasswordPolicy
	}
	if in.NewPassword == in.CurrentPassword {
		return services.ErrSamePassword
	}
	if acc.password != in.CurrentPassword {
		return services.ErrInvalidPassword
	}
	acc.password = in.NewPassword
	if in.RevokeOtherSessions {
		u.S.revokeLocked(userID, actor.SessionID)
	}
	return nil
}

func (u Users) ListSessions(_ context.Context, actor services.Actor, userID string) ([]services.Session, error) {
	_, unlock, err := u.own(actor, userID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	out := []services.Session{}
	for _, s := range u.S.sessions {
		if s.UserID == userID {
			out = append(out, services.Session{ID: s.ID, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, IPAddress: s.IPAddress, UserAgent: s.UserAgent, Current: s.ID == actor.SessionID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (u Users) RevokeSession(_ context.Context, actor services.Actor, userID, sessionID string) error {
	_, unlock, err := u.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	if sessionID == actor.SessionID {
		return services.ErrCurrentSession
	}
	for tok, s := range u.S.sessions {
		if s.UserID == userID && s.ID == sessionID {
			delete(u.S.sessions, tok)
			return nil
		}
	}
	return services.ErrSessionNotFound
}

func (u Users) RevokeOtherSessions(_ context.Context, actor services.Actor, userID string) error {
	_, unlock, err := u.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	u.S.revokeLocked(userID, actor.SessionID)
	return nil
}

func (u Users) DeleteAccount(_ context.Context, actor services.Actor, userID string) error {
	_, unlock, err := u.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	delete(u.S.accounts, userID)
	u.S.revokeLocked(userID, "")
	return nil
}

func (u Users) DataSummary(_ context.Context, actor services.Actor, userID string) (services.UserDataSummary, error) {
	acc, unlock, err := u.own(actor, userID)
	if err != nil {
		return services.UserDataSummary{}, err
	}
	defer unlock()
	sum := services.UserDataSummary{User: toService(acc)}
	for _, s := range u.S.sessions {
		if s.UserID == userID {
			sum.Sessions++
		}
	}
	return sum, nil
}

// revokeLocked deletes the user's sessions except keep.
func (s *Store) revokeLocked(userID, keep string) {
	for tok, sess := range s.sessions {
		if sess.UserID == userID && sess.ID != keep {
			delete(s.sessions, tok)
		}
	}
}

// confirmEmailPath is the preview's stand-in for auth's change-email link.
const confirmEmailPath = "/preview/confirm-email"

type emailChange struct{ userID, email, callback string }

// ConfirmEmail applies a pending email change and lands on its callbackURL,
// like auth's verification link does.
func (s *Store) ConfirmEmail(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	c, ok := s.emailChanges[r.URL.Query().Get("token")]
	if ok {
		delete(s.emailChanges, r.URL.Query().Get("token"))
		if acc := s.accounts[c.userID]; acc != nil {
			acc.user.Email = c.email
		}
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "invalid or used link", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, c.callback, http.StatusSeeOther)
}

// checkPassword applies auth's default length rules (8-128 characters).
func checkPassword(p string) error {
	switch n := len([]rune(p)); {
	case n < 8:
		return auth.ErrPasswordTooShort
	case n > 128:
		return auth.ErrPasswordTooLong
	}
	return nil
}
