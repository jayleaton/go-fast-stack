// Package uifake provides in-memory ui.Auth and ui.Users for tests and the
// preview server. It follows the auth contract's rules closely enough to
// exercise every page: sessions in a cookie, verification and reset tokens,
// and "emails" collected in an outbox instead of sent.
package uifake

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui"
)

// CookieName matches Better Auth's session cookie.
const CookieName = "better-auth.session_token"

// SessionTTL matches the auth contract default.
const SessionTTL = 7 * 24 * time.Hour

// Mail is one email the fake would have sent.
type Mail struct {
	Kind string // "verify" or "reset"
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
	outbox   []Mail
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		Now:      time.Now,
		accounts: map[string]*account{},
		sessions: map[string]ui.Session{},
		verify:   map[string]string{},
		reset:    map[string]string{},
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
func (s *Store) NewSession(userID string) ui.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newSessionLocked(userID)
}

// Outbox returns the emails sent so far.
func (s *Store) Outbox() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mail(nil), s.outbox...)
}

// Token returns the token query parameter of the last email of kind.
func (s *Store) Token(kind string) string {
	box := s.Outbox()
	for i := len(box) - 1; i >= 0; i-- {
		if box[i].Kind == kind {
			if u, err := url.Parse(box[i].URL); err == nil {
				return u.Query().Get("token")
			}
		}
	}
	return ""
}

func (s *Store) newSessionLocked(userID string) ui.Session {
	sess := ui.Session{Token: token(), UserID: userID, ExpiresAt: s.Now().Add(SessionTTL)}
	s.sessions[sess.Token] = sess
	return sess
}

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
	s.outbox = append(s.outbox, m)
	if s.OnMail != nil {
		s.OnMail(m)
	}
}

func token() string { return rand.Text() }

// Auth implements ui.Auth.
type Auth struct{ S *Store }

var _ ui.Auth = Auth{}

func (a Auth) SignUpEmail(_ context.Context, in ui.SignUpInput) (ui.User, ui.Session, error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(in.Email, "@") {
		return ui.User{}, ui.Session{}, ui.ErrInvalidEmail
	}
	if err := checkPassword(in.Password); err != nil {
		return ui.User{}, ui.Session{}, err
	}
	if existing := s.byEmailLocked(in.Email); existing != nil {
		// Like auth: with verification on, a taken email looks like success.
		if s.RequireEmailVerification {
			return ui.User{Name: in.Name, Email: in.Email}, ui.Session{}, nil
		}
		return ui.User{}, ui.Session{}, ui.ErrUserAlreadyExists
	}
	u := ui.User{ID: token(), Name: in.Name, Email: in.Email, CreatedAt: s.Now()}
	s.accounts[u.ID] = &account{user: u, password: in.Password}
	if s.RequireEmailVerification {
		tok := token()
		s.verify[tok] = u.ID
		s.mailLocked("verify", u.Email, in.CallbackURL, tok)
		return u, ui.Session{}, nil
	}
	return u, s.newSessionLocked(u.ID), nil
}

func (a Auth) SignInEmail(_ context.Context, in ui.SignInInput) (ui.User, ui.Session, error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.byEmailLocked(in.Email)
	if acc == nil || acc.password != in.Password {
		return ui.User{}, ui.Session{}, ui.ErrInvalidEmailOrPassword
	}
	if s.RequireEmailVerification && !acc.user.EmailVerified {
		return ui.User{}, ui.Session{}, ui.ErrEmailNotVerified
	}
	return acc.user, s.newSessionLocked(acc.user.ID), nil
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
		return ui.ErrInvalidToken
	}
	if err := checkPassword(newPassword); err != nil {
		return err
	}
	delete(s.reset, tok)
	s.accounts[id].password = newPassword
	return nil
}

func (a Auth) VerifyEmail(_ context.Context, tok string) (ui.User, ui.Session, error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.verify[tok]
	if !ok || s.accounts[id] == nil {
		return ui.User{}, ui.Session{}, ui.ErrInvalidToken
	}
	delete(s.verify, tok)
	acc := s.accounts[id]
	if acc.user.EmailVerified {
		return acc.user, ui.Session{}, nil
	}
	acc.user.EmailVerified = true
	return acc.user, s.newSessionLocked(id), nil
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

func (a Auth) UserFrom(ctx context.Context) (ui.User, ui.Session, bool) {
	c, ok := ctx.Value(sessionKey{}).(current)
	return c.user, c.session, ok
}

// Middleware loads the session from the cookie, like auth.Middleware.
func (a Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// Users implements ui.Users with the same rules as services.UserService.
type Users struct{ S *Store }

var _ ui.Users = Users{}

func (u Users) UpdateUserSettings(_ context.Context, actor services.Actor, userID string, in validate.UpdateUserSettings) (services.User, error) {
	if userID != actor.UserID {
		return services.User{}, &services.Error{Kind: services.KindForbidden, Message: "Cannot update another user's settings"}
	}
	s := u.S
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.accounts[userID]
	if acc == nil {
		return services.User{}, &services.Error{Kind: services.KindNotFound, Message: "User not found"}
	}
	if in.Name != nil {
		acc.user.Name = *in.Name
	}
	x := acc.user
	return services.User{ID: x.ID, Name: x.Name, Email: x.Email, EmailVerified: x.EmailVerified, CreatedAt: x.CreatedAt, UpdatedAt: s.Now()}, nil
}

func (u Users) DeleteAllUserData(_ context.Context, actor services.Actor) error {
	s := u.S
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts[actor.UserID] == nil {
		return &services.Error{Kind: services.KindNotFound, Message: "User not found"}
	}
	delete(s.accounts, actor.UserID)
	for tok, sess := range s.sessions {
		if sess.UserID == actor.UserID {
			delete(s.sessions, tok)
		}
	}
	return nil
}

// checkPassword applies auth's default length rules (8-128 characters).
func checkPassword(p string) error {
	switch n := len([]rune(p)); {
	case n < 8:
		return ui.ErrPasswordTooShort
	case n > 128:
		return ui.ErrPasswordTooLong
	}
	return nil
}
