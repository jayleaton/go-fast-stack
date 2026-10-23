package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Social sign-in: a port of Better Auth's google and apple providers and its
// /sign-in/social and /callback/{provider} endpoints, keeping the callback
// paths, state, PKCE and account-linking rules (README "Social sign-in").
//
// Two ways in:
//   - Browser (StartSocialSignIn, called by a UI handler): the state is bound
//     to a signed cookie in the browser that started, and the callback sets
//     the session cookie.
//   - App handoff (POST /sign-in/social): the app gets the provider URL and a
//     handoff secret, opens the URL in an in-app browser, and the callback
//     redirects to the app's deep link with a one-time code (60 s). The app
//     trades code + secret for a session at POST /sign-in/handoff. A session
//     token never appears in a URL, and an app that intercepts the deep link
//     cannot use the code without the secret.
//
// Stricter than Better Auth: the code flow's ID token is verified against the
// provider's keys and the flow's nonce (Better Auth only decodes it); with
// email verification required, an address the provider has not verified gets
// neither an account nor a session; and social sign-in never skips 2FA.

const (
	oauthStateTTL       = 10 * time.Minute // the verification row
	oauthStateCookieTTL = 300              // seconds: the signed <prefix>.oauth_state cookie
	oauthStateCookie    = "oauth_state"
	handoffCodeTTL      = time.Minute

	// Random lengths, in characters of stateAlphabet (6 bits each).
	stateLength         = 32 // the OAuth state
	nonceLength         = 32 // the OIDC nonce
	codeVerifierLength  = 64 // the PKCE verifier (RFC 7636: 43 to 128)
	handoffSecretLength = 43 // 258 bits
	handoffCodeLength   = 43 // 258 bits
	socialIDLength      = 32 // ids of users and accounts made here, as generateID elsewhere

	maxCallbackForm    = 64 << 10 // 64 KiB cap on Apple's form_post
	maxErrorCodeLength = 64       // longest provider error code reflected into a URL

	oauthStatePrefix  = "oauth-state:"
	handoffCodePrefix = "oauth-handoff:"
)

// ErrProviderNotFound is returned for a provider that is not configured.
var ErrProviderNotFound = &Error{http.StatusNotFound, "PROVIDER_NOT_FOUND", "Provider not found"}

// SocialSignInInput is the body of POST /sign-in/social (the app handoff).
type SocialSignInInput struct {
	Provider    string `json:"provider"`
	CallbackURL string `json:"callbackURL"` // the app's deep link, on a trusted scheme (Config.TrustedOrigins)
}

// SocialSignInStart is the answer to POST /sign-in/social. The app opens URL
// in an in-app browser and keeps HandoffSecret in memory for
// POST /sign-in/handoff.
type SocialSignInStart struct {
	URL           string `json:"url"`
	Redirect      bool   `json:"redirect"` // always false: the app opens URL itself
	HandoffSecret string `json:"handoffSecret"`
}

// HandoffInput is the body of POST /sign-in/handoff.
type HandoffInput struct {
	Code          string `json:"code"`
	HandoffSecret string `json:"handoffSecret"`
}

// oauthFailure is a refused social sign-in. The callback redirects with its
// code as ?error=<code> (Better Auth's codes, plus a few of our own).
type oauthFailure string

const (
	failAccountNotLinked  oauthFailure = "account_not_linked"
	failSignUpDisabled    oauthFailure = "signup_disabled"
	failCreateUser        oauthFailure = "unable_to_create_user"
	failLinkAccount       oauthFailure = "unable_to_link_account"
	failEmailNotVerified  oauthFailure = "email_not_verified"  // verification required, provider unverified
	failBanned            oauthFailure = "banned"              // Better Auth admin plugin's code
	failTwoFactorRequired oauthFailure = "two_factor_required" // social sign-in never skips 2FA
)

func (failure oauthFailure) Error() string { return string(failure) }

// oauthState is the verification row of one flow, named by its state.
type oauthState struct {
	Provider     string `json:"provider"`
	CallbackURL  string `json:"callbackURL"`
	CodeVerifier string `json:"codeVerifier"`
	Nonce        string `json:"nonce"`
	// HandoffHash is the SHA-256 of the app's handoff secret; empty for a
	// browser flow, which the state cookie binds instead.
	HandoffHash string `json:"handoffHash,omitempty"`
}

// handoff is the verification row of a one-time handoff code.
type handoff struct {
	UserID      string `json:"userId"`
	Provider    string `json:"provider"`
	HandoffHash string `json:"handoffHash"`
}

const stateAlphabet = idAlphabet + "-_"

const stateAlphabetMask = len(stateAlphabet) - 1 // 63: picks one of the 64 symbols from a byte

// randomString returns n characters of [a-zA-Z0-9-_] from crypto/rand
// (64 symbols: no modulo bias).
func randomString(length int) string {
	buf := make([]byte, length)
	_, _ = rand.Read(buf) // never fails on supported platforms (crypto/rand docs)
	for index, raw := range buf {
		buf[index] = stateAlphabet[int(raw)&stateAlphabetMask]
	}
	return string(buf)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// SocialProviders lists the configured provider ids in a stable order (for
// showing sign-in buttons).
func (authn *Auth) SocialProviders() []string {
	ids := []string{}
	for _, id := range []string{ProviderGoogle, ProviderApple} {
		if _, ok := authn.social.byID[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// SetSocialSecrets replaces the provider credentials (live secrets). A
// provider that is configured keeps working only with its secret set, so a
// snapshot missing one is refused and the previous secrets stay in use.
func (authn *Auth) SetSocialSecrets(creds SocialSecrets) error {
	if _, ok := authn.social.byID[ProviderGoogle]; ok && creds.GoogleClientSecret == "" {
		return errors.New("auth: Google needs a client secret")
	}
	if _, ok := authn.social.byID[ProviderApple]; ok {
		if creds.ApplePrivateKey == "" || creds.AppleKeyID == "" {
			return errors.New("auth: Apple needs a private key and its key id")
		}
		if _, err := parseES256Key(creds.ApplePrivateKey); err != nil {
			return err
		}
	}
	authn.social.secrets.Store(&creds)
	return nil
}

func (authn *Auth) provider(id string) (*socialProvider, error) {
	if provider, ok := authn.social.byID[id]; ok {
		return provider, nil
	}
	return nil, ErrProviderNotFound
}

// callbackURL is the redirect URI to register with the provider:
// <BaseURL><SocialCallbackPath><provider>, or Better Auth's
// <BaseURL>/api/auth/callback/<provider>. It is always on BaseURL
// (PUBLIC_URL), never on the request's host.
func (authn *Auth) callbackURL(provider string) string {
	if authn.cfg.SocialCallbackPath != "" {
		return authn.baseOrigin + authn.cfg.SocialCallbackPath + url.PathEscape(provider)
	}
	return authn.authURL("/callback/" + url.PathEscape(provider))
}

// SocialCallbackInput is the provider's redirect as the app received it.
type SocialCallbackInput struct {
	Provider string
	// FormPost is Apple's form_post (a cross-site POST): it is bounced to
	// the GET callback, which carries the SameSite=Lax state cookie.
	FormPost bool
	// Params are the callback parameters: the query, or the form of a
	// FormPost. Only code, state, error and user are read.
	Params url.Values
	// Cookies are the browser's cookies by name (raw values); only this
	// instance's state cookie is read.
	Cookies map[string]string
}

// SocialOutcome is where to send the browser, and the cookies to set first
// (the session, or clearing the state cookie).
type SocialOutcome struct {
	Redirect string
	Cookies  []*http.Cookie
}

// CompleteSocialSignIn finishes a flow from the provider's redirect: state
// (stored row, single use; the signed state cookie for a browser flow),
// provider error, code exchange with the PKCE verifier, ID token (signature,
// issuer, audience, expiry, nonce), account linking, then a session cookie
// (browser) or a handoff code on the app's deep link. Failures redirect to
// the error page (or the app's deep link) with ?error=<code>.
func (authn *Auth) CompleteSocialSignIn(ctx context.Context, in SocialCallbackInput) SocialOutcome {
	if in.FormPost {
		return SocialOutcome{Redirect: authn.callbackURL(in.Provider) + "?" + callbackParams(in.Params).Encode()}
	}
	var outcome SocialOutcome
	flow, failure := authn.consumeOAuthState(ctx, in.Cookies[authn.cookie(oauthStateCookie)], in.Params.Get("state"))
	if failure != "" {
		authn.emitFailure(ctx, EventSignInFailed, "", SignInMethodSocial, &Error{Code: strings.ToUpper(failure)})
		return SocialOutcome{Redirect: authn.withQuery(authn.errorPage(), "error", failure)}
	}
	if flow.HandoffHash == "" {
		outcome.Cookies = append(outcome.Cookies, authn.newSignedCookie(authn.cookie(oauthStateCookie), "", 0))
	}
	target, session, err := authn.finishSocial(ctx, in.Provider, in.Params, flow)
	if err != nil {
		code := string(internalFailure)
		if refusal, ok := errors.AsType[oauthFailure](err); ok {
			code = string(refusal)
		} else {
			authn.log.ErrorContext(ctx, "auth: social sign-in failed", "provider", flow.Provider, "err", err)
		}
		authn.emitFailure(ctx, EventSignInFailed, "", SignInMethodSocial, &Error{Code: strings.ToUpper(code)})
		errorURL := authn.errorPage()
		if flow.HandoffHash != "" {
			errorURL = flow.CallbackURL
		}
		outcome.Redirect = authn.withQuery(errorURL, "error", code)
		return outcome
	}
	if session != nil {
		outcome.Cookies = append(outcome.Cookies, authn.sessionCookie(*session))
	}
	outcome.Redirect = target
	return outcome
}

// SocialCallback is CompleteSocialSignIn for a handler that serves the
// provider's redirect itself (GET, or Apple's form_post): it only ever
// redirects. Mount it at Config.SocialCallbackPath without cross-origin
// form protection (Apple's POST is cross-site; the state check is what
// authenticates it); Handler mounts it itself when that is unset.
func (authn *Auth) SocialCallback(writer http.ResponseWriter, request *http.Request, provider string) {
	in := SocialCallbackInput{Provider: provider, Params: request.URL.Query(), Cookies: map[string]string{}}
	if request.Method == http.MethodPost {
		request.Body = http.MaxBytesReader(writer, request.Body, maxCallbackForm)
		if err := request.ParseForm(); err != nil {
			http.Redirect(writer, request, authn.withQuery(authn.errorPage(), "error", "invalid_callback_request"), http.StatusFound)
			return
		}
		in.FormPost, in.Params = true, request.Form
	}
	for _, cookie := range request.Cookies() {
		in.Cookies[cookie.Name] = cookie.Value
	}
	outcome := authn.CompleteSocialSignIn(request.Context(), in)
	for _, cookie := range outcome.Cookies {
		http.SetCookie(writer, cookie)
	}
	http.Redirect(writer, request, outcome.Redirect, http.StatusFound) //nolint:gosec // G710: targets passed checkCallbackURL when the flow started, or are ours
}

// errorPage is where browser-flow failures land with ?error=<code>.
func (authn *Auth) errorPage() string { return authn.baseOrigin + authn.cfg.SocialErrorPath }

// SocialBegin is a started browser sign-in: set Cookies, then send the
// browser to URL.
type SocialBegin struct {
	URL     string
	Cookies []*http.Cookie
}

// BeginSocialSignIn begins the browser flow: it stores the state (10 min)
// and returns the provider's authorization URL with the signed state cookie
// (5 min) that binds the flow to this browser. callbackURL is where the
// signed-in browser lands: a root-relative path or a URL on a trusted web
// origin (default "/").
func (authn *Auth) BeginSocialSignIn(ctx context.Context, provider, callbackURL string) (SocialBegin, error) {
	if callbackURL == "" {
		callbackURL = "/"
	}
	if !isSafeRelativeURL(callbackURL) && !isWebURL(callbackURL) {
		return SocialBegin{}, ErrInvalidCallbackURL
	}
	authURL, state, err := authn.startSocial(ctx, provider, callbackURL, "")
	if err != nil {
		return SocialBegin{}, err
	}
	return SocialBegin{URL: authURL, Cookies: []*http.Cookie{authn.newSignedCookie(authn.cookie(oauthStateCookie), state, oauthStateCookieTTL)}}, nil
}

// StartSocialSignIn is BeginSocialSignIn for a handler that writes the
// response itself: it sets the state cookie on writer and returns the URL to
// redirect to.
func (authn *Auth) StartSocialSignIn(ctx context.Context, writer http.ResponseWriter, provider, callbackURL string) (string, error) {
	begin, err := authn.BeginSocialSignIn(ctx, provider, callbackURL)
	if err != nil {
		return "", err
	}
	for _, cookie := range begin.Cookies {
		http.SetCookie(writer, cookie)
	}
	return begin.URL, nil
}

// StartSocialHandoff begins the app flow (POST /sign-in/social). callbackURL
// must be a deep link on a trusted app scheme: the browser that finishes
// the flow is sent there with ?code= or ?error=.
func (authn *Auth) StartSocialHandoff(ctx context.Context, in SocialSignInInput) (SocialSignInStart, error) {
	if in.CallbackURL == "" || isSafeRelativeURL(in.CallbackURL) || isWebURL(in.CallbackURL) {
		return SocialSignInStart{}, ErrInvalidCallbackURL
	}
	secret := randomString(handoffSecretLength)
	authURL, _, err := authn.startSocial(ctx, in.Provider, in.CallbackURL, sha256Hex(secret))
	if err != nil {
		return SocialSignInStart{}, err
	}
	return SocialSignInStart{URL: authURL, HandoffSecret: secret}, nil
}

func (authn *Auth) startSocial(ctx context.Context, provider, callbackURL, handoffHash string) (authURL, state string, _ error) {
	if err := authn.rateLimit(ctx, "sign-in-social", RuleSignIn); err != nil {
		return "", "", err
	}
	chosen, err := authn.provider(provider)
	if err != nil {
		return "", "", err
	}
	if err := authn.checkCallbackURL(callbackURL, ErrInvalidCallbackURL); err != nil {
		return "", "", err
	}
	now := authn.now()
	flow := oauthState{
		Provider:     chosen.id,
		CallbackURL:  callbackURL,
		CodeVerifier: randomString(codeVerifierLength),
		Nonce:        randomString(nonceLength),
		HandoffHash:  handoffHash,
	}
	state = randomString(stateLength)
	value, _ := json.Marshal(flow) //nolint:gosec // G117: the verifier and nonce are meant to be stored server-side
	if err := authn.store.createVerification(ctx, oauthStatePrefix+state, string(value), now.Add(oauthStateTTL), now); err != nil {
		return "", "", err
	}
	return chosen.authorizationURL(state, flow.CodeVerifier, flow.Nonce, authn.callbackURL(chosen.id)), state, nil
}

// isWebURL reports an absolute http(s) URL.
func isWebURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func (authn *Auth) handleSignInSocial(writer http.ResponseWriter, request *http.Request) {
	var in SocialSignInInput
	if err := authn.checkFormCSRF(request); err != nil {
		authn.writeError(writer, request, err)
		return
	}
	if err := decode(request, &in); err != nil {
		authn.writeError(writer, request, err)
		return
	}
	start, err := authn.StartSocialHandoff(request.Context(), in)
	if err != nil {
		authn.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, start)
}

func (authn *Auth) handleSocialProviders(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string][]string{"providers": authn.SocialProviders()})
}

// callbackFields are the callback parameters a form_post may carry into the
// GET callback. Apple's id_token (a bearer credential) is not one of them, so
// it never lands in a URL, history or log.
var callbackFields = []string{"code", "error", "state", "user"}

// callbackParams keeps only the callback fields of a form_post, for the
// GET it is bounced to.
func callbackParams(params url.Values) url.Values {
	kept := url.Values{}
	for _, field := range callbackFields {
		if value := params.Get(field); value != "" {
			kept.Set(field, value)
		}
	}
	return kept
}

const internalFailure oauthFailure = "internal_server_error"

func (authn *Auth) finishSocial(ctx context.Context, providerID string, query url.Values, flow oauthState) (string, *Session, error) {
	if provErr := query.Get("error"); provErr != "" {
		if !isErrorCode(provErr) {
			provErr = "oauth_error"
		}
		return "", nil, oauthFailure(provErr) // e.g. access_denied when the person cancels
	}
	provider, err := authn.provider(providerID)
	if err != nil || provider.id != flow.Provider {
		return "", nil, oauthFailure("state_mismatch")
	}
	code := query.Get("code")
	if code == "" {
		return "", nil, oauthFailure("no_code")
	}
	now := authn.now()
	tokens, err := provider.exchange(ctx, code, flow.CodeVerifier, authn.callbackURL(provider.id), now)
	if err != nil {
		authn.log.WarnContext(ctx, "auth: OAuth code exchange failed", "provider", provider.id, "err", err)
		return "", nil, oauthFailure("invalid_code")
	}
	info, err := provider.userFromIDToken(ctx, tokens.IDToken, flow.Nonce, query.Get("user"), now)
	if err != nil {
		authn.log.WarnContext(ctx, "auth: provider ID token rejected", "provider", provider.id, "err", err)
		return "", nil, oauthFailure("unable_to_get_user_info")
	}
	if info.Email == "" {
		return "", nil, oauthFailure("email_not_found")
	}
	user, err := authn.socialUser(ctx, provider, info, tokens)
	if err != nil {
		return "", nil, err
	}
	if flow.HandoffHash != "" {
		code, err := authn.issueHandoff(ctx, user.ID, provider.id, flow.HandoffHash)
		if err != nil {
			return "", nil, err
		}
		return authn.withQuery(flow.CallbackURL, "code", code), nil, nil
	}
	session, err := authn.socialSession(ctx, user, provider.id)
	if err != nil {
		return "", nil, err
	}
	return flow.CallbackURL, &session, nil
}

// isErrorCode accepts a provider's error code (RFC 6749 §4.1.2.1 uses
// lower-case words joined by "_"), so nothing else is reflected into a URL.
func isErrorCode(value string) bool {
	if value == "" || len(value) > maxErrorCodeLength {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && char != '_' {
			return false
		}
	}
	return true
}

// consumeOAuthState checks state against its row (and, for a browser flow,
// the signed state cookie) and spends it, returning an error code on
// failure. stateCookie is the raw signed cookie the browser sent. A cookie
// mismatch leaves the row, so the real flow can still finish.
func (authn *Auth) consumeOAuthState(ctx context.Context, stateCookie, state string) (oauthState, string) {
	if state == "" {
		return oauthState{}, "state_not_found"
	}
	now := authn.now()
	raw, err := authn.store.verificationValue(ctx, oauthStatePrefix+state, now)
	if err != nil {
		if !errors.Is(err, errNotFound) {
			authn.log.ErrorContext(ctx, "auth: reading OAuth state failed", "err", err)
		}
		return oauthState{}, "please_restart_the_process"
	}
	var flow oauthState
	if json.Unmarshal([]byte(raw), &flow) != nil {
		return oauthState{}, "please_restart_the_process"
	}
	if flow.HandoffHash == "" {
		cookie, ok, _ := authn.verifySignedKey(stateCookie)
		if !ok || subtle.ConstantTimeCompare([]byte(cookie), []byte(state)) != 1 {
			return oauthState{}, "state_mismatch"
		}
	}
	var spent string
	err = authn.store.tx(ctx, func(tx querier) error {
		var err error
		spent, err = authn.store.consumeVerification(ctx, tx, oauthStatePrefix+state, now)
		return err
	})
	if err != nil || spent == "" { // a concurrent callback with the same state won
		return oauthState{}, "please_restart_the_process"
	}
	return flow, ""
}

// issueHandoff stores a one-time code for userID, bound to the flow's
// handoff secret, and returns it. Only its hash is stored.
func (authn *Auth) issueHandoff(ctx context.Context, userID, provider, handoffHash string) (string, error) {
	code := randomString(handoffCodeLength)
	value, _ := json.Marshal(handoff{UserID: userID, Provider: provider, HandoffHash: handoffHash})
	now := authn.now()
	if err := authn.store.createVerification(ctx, handoffCodePrefix+sha256Hex(code), string(value), now.Add(handoffCodeTTL), now); err != nil {
		return "", err
	}
	return code, nil
}

// SignInHandoff trades a handoff code and the flow's secret for a session
// (POST /sign-in/handoff). The code is spent by the first attempt, right or
// wrong, so it cannot be guessed against; an expired, spent or mismatched
// code is ErrInvalidToken.
func (authn *Auth) SignInHandoff(ctx context.Context, in HandoffInput) (User, Session, error) {
	if err := authn.rateLimit(ctx, "sign-in-handoff", RuleSignIn); err != nil {
		return User{}, Session{}, err
	}
	if in.Code == "" || in.HandoffSecret == "" {
		return User{}, Session{}, ErrInvalidToken
	}
	var raw string
	err := authn.store.tx(ctx, func(tx querier) error {
		var err error
		raw, err = authn.store.consumeVerification(ctx, tx, handoffCodePrefix+sha256Hex(in.Code), authn.now())
		return err
	})
	var pending handoff
	if err == nil && json.Unmarshal([]byte(raw), &pending) != nil {
		err = errNotFound
	}
	if err == nil && subtle.ConstantTimeCompare([]byte(pending.HandoffHash), []byte(sha256Hex(in.HandoffSecret))) != 1 {
		err = errNotFound
	}
	if errors.Is(err, errNotFound) {
		authn.emitFailure(ctx, EventSignInFailed, pending.UserID, SignInMethodSocial, ErrInvalidToken)
		return User{}, Session{}, ErrInvalidToken
	}
	if err != nil {
		return User{}, Session{}, err
	}
	user, err := authn.store.userByID(ctx, authn.store.pool, pending.UserID)
	if err != nil {
		return User{}, Session{}, notFoundAs(err, ErrInvalidToken)
	}
	sess, err := authn.socialSession(ctx, user, pending.Provider)
	if failure, ok := errors.AsType[oauthFailure](err); ok && failure == failBanned {
		err = ErrBannedUser
	}
	if err != nil {
		authn.emitFailure(ctx, EventSignInFailed, user.ID, SignInMethodSocial, err)
		return User{}, Session{}, err
	}
	return user, sess, nil
}

func notFoundAs(err error, as error) error {
	if errors.Is(err, errNotFound) {
		return as
	}
	return err
}

func (authn *Auth) handleSignInHandoff(writer http.ResponseWriter, request *http.Request) {
	var in HandoffInput
	if err := authn.checkFormCSRF(request); err != nil {
		authn.writeError(writer, request, err)
		return
	}
	if err := decode(request, &in); err != nil {
		authn.writeError(writer, request, err)
		return
	}
	user, sess, err := authn.SignInHandoff(request.Context(), in)
	if err != nil {
		authn.writeError(writer, request, err)
		return
	}
	authn.signedIn(writer, sess)
	writeJSON(writer, http.StatusOK, map[string]any{"redirect": false, "token": sess.Token, "user": user})
}

// socialSession starts a session for a social sign-in, refusing users who
// need a second factor or are banned.
func (authn *Auth) socialSession(ctx context.Context, user User, provider string) (Session, error) {
	if authn.cfg.RequireEmailVerification && !user.EmailVerified {
		return Session{}, failEmailNotVerified
	}
	if needs, err := authn.needsSecondFactor(ctx, user.ID); err != nil {
		return Session{}, err
	} else if needs {
		return Session{}, failTwoFactorRequired
	}
	sess, err := authn.createSession(ctx, user.ID)
	if errors.Is(err, ErrBannedUser) {
		return Session{}, failBanned
	}
	if err != nil {
		return Session{}, err
	}
	authn.emit(ctx, Event{Kind: EventSignIn, UserID: user.ID, Email: user.Email, SessionID: sess.ID, Method: SignInMethodSocial, Details: map[string]string{"provider": provider}})
	return sess, nil
}

// socialUser is Better Auth's handleOAuthUserInfo: the user already linked
// to this provider account, else an existing user with the same email when
// both the provider and the user have verified it (the account is linked),
// else a new user. Refusals are oauthFailure values.
func (authn *Auth) socialUser(ctx context.Context, provider *socialProvider, info socialUser, tokens oauthTokens) (User, error) {
	user, err := authn.resolveSocialUser(ctx, provider, info, tokens)
	if isUniqueViolation(err) { // a concurrent first sign-in created it: use that
		user, err = authn.resolveSocialUser(ctx, provider, info, tokens)
	}
	if isUniqueViolation(err) {
		err = failCreateUser
	}
	return user, err
}

func (authn *Auth) resolveSocialUser(ctx context.Context, provider *socialProvider, info socialUser, tokens oauthTokens) (User, error) {
	email := strings.ToLower(info.Email)
	now := authn.now()
	acc := socialAccount{ProviderID: provider.id, AccountID: info.ID, Tokens: tokens}
	user, accountID, err := authn.store.userByAccount(ctx, provider.id, info.ID)
	switch {
	case err == nil: // linked: refresh tokens, maybe mark verified
		if err := authn.store.updateAccountTokens(ctx, accountID, tokens, now); err != nil {
			return User{}, err
		}
		if info.EmailVerified && !user.EmailVerified && email == user.Email {
			return authn.store.markVerified(ctx, user.ID, now)
		}
		return user, nil
	case !errors.Is(err, errNotFound):
		return User{}, err
	}
	user, err = authn.store.userByEmail(ctx, email)
	switch {
	case err == nil: // same email, not linked yet
		if !info.EmailVerified || !user.EmailVerified {
			return User{}, failAccountNotLinked
		}
		if err := authn.store.insertAccount(ctx, authn.store.pool, user.ID, acc, now); err != nil {
			if isUniqueViolation(err) {
				return User{}, err
			}
			authn.log.ErrorContext(ctx, "auth: linking account failed", "err", err)
			return User{}, failLinkAccount
		}
		authn.emit(ctx, Event{Kind: EventAccountLinked, UserID: user.ID, Email: user.Email, Details: map[string]string{"provider": provider.id}})
		return user, nil
	case !errors.Is(err, errNotFound):
		return User{}, err
	}
	if authn.cfg.DisableSignUp {
		return User{}, failSignUpDisabled
	}
	if authn.cfg.RequireEmailVerification && !info.EmailVerified {
		// Better Auth would create the user and sign them in. Here an
		// unproven address gets nothing, so it cannot squat the account of
		// the inbox's real owner.
		return User{}, failEmailNotVerified
	}
	user = User{ID: generateID(socialIDLength), Name: info.Name, Email: email, EmailVerified: info.EmailVerified, Image: info.Image, CreatedAt: now, UpdatedAt: now}
	err = authn.store.tx(ctx, func(tx querier) error {
		if err := authn.store.createUser(ctx, tx, user); err != nil {
			return err
		}
		return authn.store.insertAccount(ctx, tx, user.ID, acc, now)
	})
	if isUniqueViolation(err) {
		return User{}, err
	}
	if err != nil {
		authn.log.ErrorContext(ctx, "auth: creating social user failed", "err", err)
		return User{}, failCreateUser
	}
	return user, nil
}

// socialAccount is an "account" row for a provider.
type socialAccount struct {
	ProviderID, AccountID string
	Tokens                oauthTokens
}

func scopeColumn(tokens oauthTokens) *string {
	if len(tokens.Scopes) == 0 {
		return nil
	}
	value := strings.Join(tokens.Scopes, ",")
	return &value
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// userByAccount returns the user linked to a provider account and the
// account row's id.
func (storage *store) userByAccount(ctx context.Context, providerID, accountID string) (User, string, error) {
	var id string
	user, err := scanUser(storage.pool.QueryRow(ctx, `
		SELECT `+userColumns+`, a."id"
		FROM "account" a JOIN "user" u ON u."id" = a."userId"
		WHERE a."providerId" = $1 AND a."accountId" = $2`, providerID, accountID), &id)
	return user, id, err
}

func (storage *store) insertAccount(ctx context.Context, tx querier, userID string, acc socialAccount, now time.Time) error {
	tokens := acc.Tokens
	_, err := tx.Exec(ctx, `
		INSERT INTO "account" ("id", "accountId", "providerId", "userId", "accessToken", "refreshToken", "idToken",
			"accessTokenExpiresAt", "refreshTokenExpiresAt", "scope", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		generateID(socialIDLength), acc.AccountID, acc.ProviderID, userID, optional(tokens.AccessToken), optional(tokens.RefreshToken), optional(tokens.IDToken),
		tokens.AccessTokenExpiresAt, tokens.RefreshTokenExpiresAt, scopeColumn(tokens), now)
	return err
}

// updateAccountTokens stores fresh tokens, keeping columns the provider did
// not send this time (Google sends no refresh token on later sign-ins).
func (storage *store) updateAccountTokens(ctx context.Context, id string, tokens oauthTokens, now time.Time) error {
	_, err := storage.pool.Exec(ctx, `
		UPDATE "account" SET
			"accessToken"           = COALESCE($2, "accessToken"),
			"refreshToken"          = COALESCE($3, "refreshToken"),
			"idToken"               = COALESCE($4, "idToken"),
			"accessTokenExpiresAt"  = COALESCE($5, "accessTokenExpiresAt"),
			"refreshTokenExpiresAt" = COALESCE($6, "refreshTokenExpiresAt"),
			"scope"                 = COALESCE($7, "scope"),
			"updatedAt"             = $8
		WHERE "id" = $1`,
		id, optional(tokens.AccessToken), optional(tokens.RefreshToken), optional(tokens.IDToken), tokens.AccessTokenExpiresAt, tokens.RefreshTokenExpiresAt, scopeColumn(tokens), now)
	return err
}

// markVerified sets emailVerified and returns the user.
func (storage *store) markVerified(ctx context.Context, userID string, now time.Time) (User, error) {
	return scanUser(storage.pool.QueryRow(ctx, `
		UPDATE "user" u SET "emailVerified" = true, "updatedAt" = $2
		WHERE u."id" = $1
		RETURNING `+userColumns, userID, now))
}
