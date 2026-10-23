package validate

import "strings"

// CreateImageFromURL is the POST /images/import body. Which URLs the server
// may fetch is a business rule (services.FetchURLAllowed); this checks shape.
type CreateImageFromURL struct {
	URL string `json:"url"`
}

// Validate trims the URL and checks it is an absolute http(s) URL.
func (input *CreateImageFromURL) Validate() error {
	input.URL = strings.TrimSpace(input.URL)
	fields := Fields{}
	fields.Add(!isHTTPURL(input.URL), "url", "must be an http(s) URL of at most 2048 characters")
	return fields.Err()
}
