// The preview's in-memory backend: ui.Auth and ui.Users with packages/auth's
// and services.UserService's rules, close enough to click through every page.
// Sessions live in a cookie, tokens in maps, and "emails" are logged.
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

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
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
	user     auth.User
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
	sessions map[string]auth.Session
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
		sessions:     map[string]auth.Session{},
		verify:       map[string]string{},
		reset:        map[string]string{},
		emailChanges: map[string]emailChange{},
	}
}

// AddUser creates an account directly (seed data).
func (store *Store) AddUser(name, email, password string, verified bool) auth.User {
	store.mu.Lock()
	defer store.mu.Unlock()
	user := auth.User{ID: token(), Name: name, Email: email, EmailVerified: verified, CreatedAt: store.Now()}
	store.accounts[user.ID] = &account{user: user, password: password}
	return user
}

// NewSession signs a user in directly (tests).
func (store *Store) NewSession(userID, userAgent, ip string, age time.Duration) auth.Session {
	store.mu.Lock()
	defer store.mu.Unlock()
	ctx := context.WithValue(context.Background(), metaKey{}, requestMeta{userAgent, ip})
	sess := store.newSessionLocked(ctx, userID)
	sess.CreatedAt = sess.CreatedAt.Add(-age)
	store.sessions[sess.Token] = sess
	return sess
}

func (store *Store) newSessionLocked(ctx context.Context, userID string) auth.Session {
	now := store.Now()
	sess := auth.Session{ID: token(), Token: token(), UserID: userID, CreatedAt: now, ExpiresAt: now.Add(SessionTTL)}
	if meta, ok := ctx.Value(metaKey{}).(requestMeta); ok {
		sess.UserAgent, sess.IPAddress = &meta.userAgent, &meta.ip
	}
	store.sessions[sess.Token] = sess
	return sess
}

type metaKey struct{}

type requestMeta struct{ userAgent, ip string }

func (store *Store) byEmailLocked(email string) *account {
	for _, account := range store.accounts {
		if strings.EqualFold(account.user.Email, email) {
			return account
		}
	}
	return nil
}

func (store *Store) mailLocked(kind, to, path string, tok string) {
	mail := Mail{Kind: kind, To: to, URL: path + "?" + url.Values{"token": {tok}}.Encode()}
	if store.OnMail != nil {
		store.OnMail(mail)
	}
}

func token() string { return rand.Text() }

// Auth implements ui.Auth.
type Auth struct{ Store *Store }

func (fake Auth) SignUpEmail(ctx context.Context, in auth.SignUpInput) (auth.User, auth.Session, error) {
	store := fake.Store
	store.mu.Lock()
	defer store.mu.Unlock()
	if !strings.Contains(in.Email, "@") {
		return auth.User{}, auth.Session{}, auth.ErrInvalidEmail
	}
	if err := checkPassword(in.Password); err != nil {
		return auth.User{}, auth.Session{}, err
	}
	if existing := store.byEmailLocked(in.Email); existing != nil {
		// Like auth: with verification on, a taken email looks like success.
		if store.RequireEmailVerification {
			return auth.User{Name: in.Name, Email: in.Email}, auth.Session{}, nil
		}
		return auth.User{}, auth.Session{}, auth.ErrUserAlreadyExists
	}
	user := auth.User{ID: token(), Name: in.Name, Email: in.Email, CreatedAt: store.Now()}
	store.accounts[user.ID] = &account{user: user, password: in.Password}
	if store.RequireEmailVerification {
		tok := token()
		store.verify[tok] = user.ID
		store.mailLocked("verify", user.Email, in.CallbackURL, tok)
		return user, auth.Session{}, nil
	}
	return user, store.newSessionLocked(ctx, user.ID), nil
}

func (fake Auth) SignInEmail(ctx context.Context, in auth.SignInInput) (auth.User, auth.Session, error) {
	store := fake.Store
	store.mu.Lock()
	defer store.mu.Unlock()
	acc := store.byEmailLocked(in.Email)
	if acc == nil || acc.password != in.Password {
		return auth.User{}, auth.Session{}, auth.ErrInvalidEmailOrPassword
	}
	if store.RequireEmailVerification && !acc.user.EmailVerified {
		return auth.User{}, auth.Session{}, auth.ErrEmailNotVerified
	}
	return acc.user, store.newSessionLocked(ctx, acc.user.ID), nil
}

func (fake Auth) SignOut(_ context.Context, sessionToken string) error {
	fake.Store.mu.Lock()
	defer fake.Store.mu.Unlock()
	delete(fake.Store.sessions, sessionToken)
	return nil
}

func (fake Auth) RequestPasswordReset(_ context.Context, email, redirectTo string) error {
	store := fake.Store
	store.mu.Lock()
	defer store.mu.Unlock()
	// Unknown emails succeed silently, so the form does not reveal accounts.
	if acc := store.byEmailLocked(email); acc != nil {
		tok := token()
		store.reset[tok] = acc.user.ID
		store.mailLocked("reset", email, redirectTo, tok)
	}
	return nil
}

func (fake Auth) ResetPassword(_ context.Context, tok, newPassword string) error {
	store := fake.Store
	store.mu.Lock()
	defer store.mu.Unlock()
	id, ok := store.reset[tok]
	if !ok || store.accounts[id] == nil {
		return auth.ErrInvalidToken
	}
	if err := checkPassword(newPassword); err != nil {
		return err
	}
	delete(store.reset, tok)
	store.accounts[id].password = newPassword
	return nil
}

func (fake Auth) VerifyEmail(ctx context.Context, tok string) (auth.User, auth.Session, error) {
	store := fake.Store
	store.mu.Lock()
	defer store.mu.Unlock()
	id, ok := store.verify[tok]
	if !ok || store.accounts[id] == nil {
		return auth.User{}, auth.Session{}, auth.ErrInvalidToken
	}
	delete(store.verify, tok)
	acc := store.accounts[id]
	if acc.user.EmailVerified {
		return acc.user, auth.Session{}, nil
	}
	acc.user.EmailVerified = true
	return acc.user, store.newSessionLocked(ctx, id), nil
}

// CheckCallbackURL trusts this site's paths only: the preview has no mobile app.
func (Auth) CheckCallbackURL(callbackURL string) error {
	if strings.HasPrefix(callbackURL, "/") && !strings.HasPrefix(callbackURL, "//") {
		return nil
	}
	return errors.New("untrusted callback")
}

func (fake Auth) SendVerificationEmail(_ context.Context, email, callbackURL string) error {
	store := fake.Store
	store.mu.Lock()
	defer store.mu.Unlock()
	if acc := store.byEmailLocked(email); acc != nil && !acc.user.EmailVerified {
		tok := token()
		store.verify[tok] = acc.user.ID
		store.mailLocked("verify", email, callbackURL, tok)
	}
	return nil
}

func (fake Auth) SetSessionCookie(writer http.ResponseWriter, sess auth.Session) {
	http.SetCookie(writer, &http.Cookie{
		Name:     CookieName,
		Value:    sess.Token,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (fake Auth) ClearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

type sessionKey struct{}

type current struct {
	user    auth.User
	session auth.Session
}

// Caller reads the preview's session for its access gate
// (access.FromContext), like server.Callers over auth.Middleware.
func (fake Auth) Caller(ctx context.Context) (access.Caller, bool) {
	signedIn, ok := ctx.Value(sessionKey{}).(current)
	if !ok {
		return access.Caller{}, false
	}
	return access.Caller{
		User:    services.User(signedIn.user),
		Session: access.Session{ID: signedIn.session.ID, UserID: signedIn.session.UserID, ExpiresAt: signedIn.session.ExpiresAt, Token: signedIn.session.Token},
	}, true
}

// Middleware loads the session from the cookie, like auth.Middleware.
func (fake Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ip, _, _ := strings.Cut(request.RemoteAddr, ":")
		request = request.WithContext(context.WithValue(request.Context(), metaKey{}, requestMeta{request.UserAgent(), ip}))
		if cookie, err := request.Cookie(CookieName); err == nil {
			store := fake.Store
			store.mu.Lock()
			sess, ok := store.sessions[cookie.Value]
			var acc *account
			if ok && sess.ExpiresAt.After(store.Now()) {
				acc = store.accounts[sess.UserID]
			}
			var cur current
			if acc != nil {
				cur = current{user: acc.user, session: sess}
			}
			store.mu.Unlock()
			if acc != nil {
				request = request.WithContext(context.WithValue(request.Context(), sessionKey{}, cur))
			}
		}
		next.ServeHTTP(writer, request)
	})
}

// Users implements api.UserService with services.UserService's rules; the
// preview serves the real API endpoints over it.
type Users struct{ Store *Store }

var _ api.UserService = Users{}

// own locks the store and returns the actor's account, or the service error.
func (users Users) own(actor services.Actor, userID string) (*account, func(), error) {
	if actor.UserID == "" || actor.UserID != userID {
		return nil, nil, services.ErrForbidden
	}
	users.Store.mu.Lock()
	acc := users.Store.accounts[userID]
	if acc == nil {
		users.Store.mu.Unlock()
		return nil, nil, services.ErrUserNotFound
	}
	return acc, users.Store.mu.Unlock, nil
}

func toService(account *account) services.User {
	user := account.user
	return services.User{ID: user.ID, Name: user.Name, Email: user.Email, EmailVerified: user.EmailVerified, Image: user.Image, CreatedAt: user.CreatedAt, UpdatedAt: user.UpdatedAt}
}

func (users Users) GetProfile(_ context.Context, actor services.Actor, userID string) (services.User, error) {
	acc, unlock, err := users.own(actor, userID)
	if err != nil {
		return services.User{}, err
	}
	defer unlock()
	return toService(acc), nil
}

func (users Users) UpdateProfile(_ context.Context, actor services.Actor, userID string, in validate.UpdateUserSettings) (services.User, error) {
	acc, unlock, err := users.own(actor, userID)
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
	acc.user.UpdatedAt = users.Store.Now()
	return toService(acc), nil
}

func (users Users) RequestEmailChange(_ context.Context, actor services.Actor, userID string, in validate.UpdateUserEmail) error {
	acc, unlock, err := users.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	if strings.EqualFold(in.NewEmail, acc.user.Email) {
		return services.ErrSameEmail
	}
	if other := users.Store.byEmailLocked(in.NewEmail); other != nil {
		return services.ErrEmailTaken
	}
	tok := token()
	users.Store.emailChanges[tok] = emailChange{userID: userID, email: in.NewEmail, callback: in.CallbackURL}
	users.Store.mailLocked("change-email", in.NewEmail, confirmEmailPath, tok)
	return nil
}

func (users Users) ChangePassword(_ context.Context, actor services.Actor, userID string, in validate.UpdateUserPassword) error {
	acc, unlock, err := users.own(actor, userID)
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
		users.Store.revokeLocked(userID, actor.SessionID)
	}
	return nil
}

func (users Users) ListSessions(_ context.Context, actor services.Actor, userID string) ([]services.Session, error) {
	_, unlock, err := users.own(actor, userID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	out := []services.Session{}
	for _, session := range users.Store.sessions {
		if session.UserID == userID {
			out = append(out, services.Session{ID: session.ID, CreatedAt: session.CreatedAt, ExpiresAt: session.ExpiresAt, IPAddress: session.IPAddress, UserAgent: session.UserAgent, Current: session.ID == actor.SessionID})
		}
	}
	sort.Slice(out, func(index, inner int) bool { return out[index].CreatedAt.After(out[inner].CreatedAt) })
	return out, nil
}

func (users Users) RevokeSession(_ context.Context, actor services.Actor, userID, sessionID string) error {
	_, unlock, err := users.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	if sessionID == actor.SessionID {
		return services.ErrCurrentSession
	}
	for tok, session := range users.Store.sessions {
		if session.UserID == userID && session.ID == sessionID {
			delete(users.Store.sessions, tok)
			return nil
		}
	}
	return services.ErrSessionNotFound
}

func (users Users) RevokeOtherSessions(_ context.Context, actor services.Actor, userID string) error {
	_, unlock, err := users.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	users.Store.revokeLocked(userID, actor.SessionID)
	return nil
}

func (users Users) DeleteAccount(_ context.Context, actor services.Actor, userID string) error {
	_, unlock, err := users.own(actor, userID)
	if err != nil {
		return err
	}
	defer unlock()
	delete(users.Store.accounts, userID)
	users.Store.revokeLocked(userID, "")
	return nil
}

func (users Users) DataSummary(_ context.Context, actor services.Actor, userID string) (services.UserDataSummary, error) {
	acc, unlock, err := users.own(actor, userID)
	if err != nil {
		return services.UserDataSummary{}, err
	}
	defer unlock()
	sum := services.UserDataSummary{User: toService(acc)}
	for _, session := range users.Store.sessions {
		if session.UserID == userID {
			sum.Sessions++
		}
	}
	return sum, nil
}

// revokeLocked deletes the user's sessions except keep.
func (store *Store) revokeLocked(userID, keep string) {
	for tok, sess := range store.sessions {
		if sess.UserID == userID && sess.ID != keep {
			delete(store.sessions, tok)
		}
	}
}

// confirmEmailPath is the preview's stand-in for auth's change-email link.
const confirmEmailPath = "/preview/confirm-email"

type emailChange struct{ userID, email, callback string }

// ConfirmEmail applies a pending email change and lands on its callbackURL,
// like auth's verification link does.
func (store *Store) ConfirmEmail(writer http.ResponseWriter, request *http.Request) {
	store.mu.Lock()
	change, ok := store.emailChanges[request.URL.Query().Get("token")]
	if ok {
		delete(store.emailChanges, request.URL.Query().Get("token"))
		if acc := store.accounts[change.userID]; acc != nil {
			acc.user.Email = change.email
		}
	}
	store.mu.Unlock()
	if !ok {
		http.Error(writer, "invalid or used link", http.StatusBadRequest)
		return
	}
	http.Redirect(writer, request, change.callback, http.StatusSeeOther)
}

// checkPassword applies auth's default length rules (8-128 characters).
func checkPassword(path string) error {
	switch count := len([]rune(path)); {
	case count < auth.DefaultMinPasswordLength:
		return auth.ErrPasswordTooShort
	case count > auth.DefaultMaxPasswordLength:
		return auth.ErrPasswordTooLong
	}
	return nil
}
