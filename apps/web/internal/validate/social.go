package validate

// Limits on social sign-in inputs.
const (
	maxSocialCallbackURL = 2048    // longest callbackURL accepted
	maxSocialParam       = 8 << 10 // 8 KiB: longest provider callback parameter (Apple's "user" JSON is far smaller)
	maxSocialCookies     = 64      // cookies a browser sends one site at most
)

// StartSocialSignIn is the POST /auth/social/{provider}/start body.
type StartSocialSignIn struct {
	// CallbackURL is where the signed-in browser lands: a page on this site
	// (default "/").
	CallbackURL string `json:"callbackURL"`
}

// Validate checks the callback's length; auth decides whether it is trusted.
func (in *StartSocialSignIn) Validate() error {
	fields := Fields{}
	fields.Add(len(in.CallbackURL) > maxSocialCallbackURL, "callbackURL", "must be at most 2048 characters")
	return fields.Err()
}

// FinishSocialSignIn is the POST /auth/social/{provider}/finish body: the
// provider's redirect as the browser delivered it to the UI's callback page.
type FinishSocialSignIn struct {
	// FormPost is Apple's form_post (a cross-site POST to the callback).
	FormPost bool `json:"formPost"`
	// Params are the callback parameters (code, state, error, user).
	Params map[string]string `json:"params"`
	// Cookies are the browser's cookies by name, raw (the signed state
	// cookie binds the flow to the browser that started it).
	Cookies map[string]string `json:"cookies"`
}

// Validate bounds the sizes; the state check is auth's.
func (in *FinishSocialSignIn) Validate() error {
	fields := Fields{}
	for name, value := range in.Params {
		fields.Add(len(value) > maxSocialParam, "params."+name, "is too long")
	}
	fields.Add(len(in.Cookies) > maxSocialCookies, "cookies", "too many cookies")
	return fields.Err()
}
