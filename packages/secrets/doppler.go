package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultDopplerURL is the Doppler API origin used when Doppler.APIURL is empty.
const DefaultDopplerURL = "https://api.doppler.com"

// dopplerDownloadPath is the Download API
// (https://docs.doppler.com/reference/secrets-download). A service token is
// scoped to one project config, so neither is sent.
const dopplerDownloadPath = "/v3/configs/config/secrets/download?format=json&include_dynamic_secrets=false"

// maxDopplerBody bounds the response; a config is a few KiB.
const maxDopplerBody = 1 << 20

// dopplerTimeout bounds one call to the Doppler API.
const dopplerTimeout = 15 * time.Second

// Doppler fetches a config's secrets with a read-only service token over
// plain net/http (Doppler has no maintained Go SDK). One request returns the
// whole config; when Doppler sends an ETag the next request is conditional and
// a 304 keeps the previous snapshot.
type Doppler struct {
	Token      Secret       // DOPPLER_TOKEN: a read-only service token (dp.st.…) for one config
	APIURL     string       // defaults to DefaultDopplerURL; tests point it at a fake
	HTTPClient *http.Client // defaults to a client with a 15s timeout
}

var _ Provider = (*Doppler)(nil)

// NewDoppler returns a Doppler provider for token against the public API.
func NewDoppler(token Secret) *Doppler {
	return &Doppler{Token: token, APIURL: DefaultDopplerURL, HTTPClient: newDopplerClient()}
}

// newDopplerClient never follows redirects: Go re-sends Authorization on a
// same-host redirect even from https to http, and the API has no reason to
// redirect. A 3xx becomes an "unexpected status" error.
func newDopplerClient() *http.Client {
	return &http.Client{
		Timeout:       dopplerTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Format redacts the token, so %+v of the provider is safe to log.
func (doppler *Doppler) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "secrets.Doppler{APIURL: %q, Token: %s}", doppler.APIURL, redacted)
}

// LogValue implements slog.LogValuer without the token.
func (doppler *Doppler) LogValue() slog.Value { return slog.StringValue("doppler " + doppler.APIURL) }

// Name returns "doppler".
func (*Doppler) Name() string { return "doppler" }

// Fetch downloads the config. Errors carry the status code only, never the
// token or the response body.
func (doppler *Doppler) Fetch(ctx context.Context, previous Snapshot) (Snapshot, error) {
	if doppler.Token.IsZero() {
		return Snapshot{}, errors.New("doppler: missing token")
	}
	base := doppler.APIURL
	if base == "" {
		base = DefaultDopplerURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+dopplerDownloadPath, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("doppler: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+doppler.Token.value)
	req.Header.Set("Accept", "application/json")
	if previous.Len() > 0 && previous.Revision() != "" {
		req.Header.Set("If-None-Match", previous.Revision())
	}

	client := doppler.HTTPClient
	if client == nil {
		client = newDopplerClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("doppler: request: %w", redactURLError(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDopplerBody))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotModified && previous.Len() > 0:
		return previous, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return Snapshot{}, &rateLimited{wait: parseRetryAfter(resp.Header.Get("Retry-After"))}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Snapshot{}, fmt.Errorf("doppler: token rejected (%s): check DOPPLER_TOKEN is a valid service token", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return Snapshot{}, fmt.Errorf("doppler: unexpected status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDopplerBody+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("doppler: read response: %w", err)
	}
	if len(body) > maxDopplerBody {
		return Snapshot{}, fmt.Errorf("doppler: response larger than %d bytes", maxDopplerBody)
	}
	values, err := decodeDoppler(body)
	if err != nil {
		return Snapshot{}, err
	}
	return NewSnapshot(values, resp.Header.Get("ETag")), nil
}

// decodeDoppler accepts exactly one JSON object of string values. Errors name
// the key at most, never a value.
func decodeDoppler(body []byte) (map[string]string, error) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil || raw == nil {
		return nil, errors.New("doppler: malformed response: want a JSON object of string values")
	}
	if dec.More() {
		return nil, errors.New("doppler: malformed response: trailing data after the JSON object")
	}
	values := make(map[string]string, len(raw))
	for key, message := range raw {
		var text string
		if err := json.Unmarshal(message, &text); err != nil {
			return nil, fmt.Errorf("doppler: malformed response: %s is not a string", key)
		}
		values[key] = text
	}
	return values, nil
}

type rateLimited struct{ wait time.Duration }

func (limited *rateLimited) Error() string {
	return fmt.Sprintf("doppler: rate limited (429), retry after %s", limited.wait)
}
func (limited *rateLimited) RetryAfter() time.Duration { return limited.wait }

// parseRetryAfter reads delta-seconds or an HTTP date; unknown means 0.
func parseRetryAfter(value string) time.Duration {
	if size, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && size > 0 {
		return time.Duration(size) * time.Second
	}
	if moment, err := http.ParseTime(value); err == nil {
		return max(time.Until(moment), 0)
	}
	return 0
}

// redactURLError drops the URL from a transport error; it never holds the
// token, but a developer may have put credentials in DOPPLER_API_URL.
func redactURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}
