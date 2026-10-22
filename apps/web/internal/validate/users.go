package validate

import (
	"net/url"
	"strings"
	"unicode/utf8"
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
