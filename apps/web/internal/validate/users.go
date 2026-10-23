package validate

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
)

// Length limits; each error message states the same number.
const (
	maxNameLength  = 255  // a display name, in characters
	maxEmailLength = 255  // an email address, in bytes
	maxURLLength   = 2048 // an image or callback URL, in bytes
)

// UpdateUserSettings is the PATCH body for a user. Email and password changes
// belong to the auth flows (they need verification), not this schema.
type UpdateUserSettings struct {
	Name  *string          `json:"name"`
	Image Nullable[string] `json:"image"`
}

// Validate trims the name and checks lengths and the image URL.
func (settings *UpdateUserSettings) Validate() error {
	fields := Fields{}
	if settings.Name != nil {
		name := strings.TrimSpace(*settings.Name)
		settings.Name = &name
		fields.Add(name == "" || utf8.RuneCountInString(name) > maxNameLength, "name", "must be 1-255 characters")
	}
	if settings.Image.Value != nil {
		fields.Add(!isHTTPURL(*settings.Image.Value), "image", "must be an http(s) URL")
	}
	return fields.Err()
}

func isHTTPURL(raw string) bool {
	link, err := url.Parse(raw)
	return err == nil && (link.Scheme == "http" || link.Scheme == "https") && link.Host != "" && len(raw) <= maxURLLength
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
func (email *UpdateUserEmail) Validate() error {
	email.NewEmail = strings.ToLower(strings.TrimSpace(email.NewEmail))
	if email.CallbackURL == "" {
		email.CallbackURL = config.AppRoutes.EmailChanged
	}
	fields := Fields{}
	fields.Add(!isEmail(email.NewEmail), "newEmail", "must be a valid email address")
	fields.Add(len(email.CallbackURL) > maxURLLength, "callbackURL", "must be at most 2048 characters")
	return fields.Err()
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
func (password *UpdateUserPassword) Validate() error {
	fields := Fields{}
	fields.Add(password.CurrentPassword == "", "currentPassword", "is required")
	fields.Add(password.NewPassword == "", "newPassword", "is required")
	return fields.Err()
}

func isEmail(text string) bool {
	address, err := mail.ParseAddress(text)
	return err == nil && address.Address == text && len(text) <= maxEmailLength
}
