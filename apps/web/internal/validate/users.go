package validate

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
)

// UpdateUserSettings is the PATCH body for a user. Email and password changes
// belong to the auth flows (they need verification), not this schema.
type UpdateUserSettings struct {
	Name  *string          `json:"name"`
	Image Nullable[string] `json:"image"`
}

// Validate trims the name and checks lengths and the image URL.
func (u *UpdateUserSettings) Validate() error {
	f := Fields{}
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		u.Name = &name
		f.Add(name == "" || utf8.RuneCountInString(name) > 255, "name", "must be 1-255 characters")
	}
	if u.Image.Value != nil {
		f.Add(!isHTTPURL(*u.Image.Value), "image", "must be an http(s) URL")
	}
	return f.Err()
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && len(raw) <= 2048
}

// UpdateUserEmail is the POST /me/email body (Better Auth change-email names).
// The address changes only after the link sent to NewEmail is opened.
type UpdateUserEmail struct {
	NewEmail string `json:"newEmail"`
	// CallbackURL is where the verification link lands; config.AppRoutes.EmailChanged when empty.
	CallbackURL string `json:"callbackURL"`
}

// Validate normalises the address and checks its shape. Whether it differs
// from the current address is a business rule (services.UserService).
func (u *UpdateUserEmail) Validate() error {
	u.NewEmail = strings.ToLower(strings.TrimSpace(u.NewEmail))
	if u.CallbackURL == "" {
		u.CallbackURL = config.AppRoutes.EmailChanged
	}
	f := Fields{}
	f.Add(!isEmail(u.NewEmail), "newEmail", "must be a valid email address")
	f.Add(len(u.CallbackURL) > 2048, "callbackURL", "must be at most 2048 characters")
	return f.Err()
}

// UpdateUserPassword is the POST /me/password body (Better Auth
// change-password names). The password policy is a business rule
// (services.UserService); this only checks that both fields are present.
type UpdateUserPassword struct {
	CurrentPassword     string `json:"currentPassword"`
	NewPassword         string `json:"newPassword"`
	RevokeOtherSessions bool   `json:"revokeOtherSessions"`
}

// Validate checks that both passwords are present.
func (u *UpdateUserPassword) Validate() error {
	f := Fields{}
	f.Add(u.CurrentPassword == "", "currentPassword", "is required")
	f.Add(u.NewPassword == "", "newPassword", "is required")
	return f.Err()
}

func isEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && len(s) <= 255
}
