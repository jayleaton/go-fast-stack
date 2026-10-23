// Package safefetch GETs URLs that come from users (import an image by URL,
// link previews, webhooks) without letting them reach private networks
// (SSRF), hang, or return unbounded bodies.
//
// The rules are the caller's: Policy.AllowAddr decides which IPs may be
// dialled and Policy.CheckURL which URLs may be requested, so the app's
// service layer owns (and tests) them; this package enforces them:
//
//   - AllowAddr runs at dial time on every IP the name resolved to, and again
//     in net.Dialer.Control on the exact socket address before connect(2), on
//     every connection including each redirect hop. A name that resolves to a
//     public IP when the URL is checked and to 127.0.0.1 when it is dialled
//     (DNS rebinding) is refused.
//   - CheckURL runs on the first URL and on every redirect target; redirects
//     are capped, https never downgrades to http, and credentials and Referer
//     are never forwarded.
//   - No proxy, ever (a proxy would dial the target for us, unchecked).
//   - One deadline covers the whole request; the body is counted as it
//     streams and refused past MaxBytes; no transparent decompression.
//
// Build one Client at startup and share it (safe for concurrent use).
package safefetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"syscall"
	"time"
)

// Errors from Fetch; match with errors.Is. Messages never contain URL
// credentials or query strings.
var (
	ErrBlockedAddress   = errors.New("safefetch: address not allowed")
	ErrBlockedURL       = errors.New("safefetch: url not allowed")
	ErrTooManyRedirects = errors.New("safefetch: too many redirects")
	ErrTooLarge         = errors.New("safefetch: response too large")
	ErrTimeout          = errors.New("safefetch: timed out")
	ErrStatus           = errors.New("safefetch: unexpected status") // a non-2xx final response; the body is discarded
	ErrFetch            = errors.New("safefetch: fetch failed")      // DNS, refused connection, TLS, malformed response
)

// Defaults for zero Policy fields, and the transport's fixed limits.
const (
	DefaultMaxBytes = 10 << 20         // 10 MiB
	DefaultTimeout  = 30 * time.Second // whole request

	dialTimeout            = 10 * time.Second
	tcpKeepAlive           = 30 * time.Second
	tlsHandshakeTimeout    = 10 * time.Second
	responseHeaderTimeout  = 15 * time.Second
	maxIdleConns           = 16               // pooled keep-alive connections, all already vetted when dialled
	idleConnTimeout        = 30 * time.Second // drop pooled connections soon: they were vetted at dial time only
	maxResponseHeaderBytes = 64 << 10         // 64 KiB
	portBase, portBits     = 10, 16           // a port is a decimal uint16
)

// Resolver looks up a host name; *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Policy is what a Client may fetch. AllowAddr and CheckURL are required.
type Policy struct {
	AllowAddr    func(netip.Addr) bool // may this IP be dialled?
	CheckURL     func(*url.URL) error  // may this URL (or redirect target) be requested?
	MaxRedirects int                   // redirects followed (0 follows none)
	MaxBytes     int64                 // body cap (<= 0: DefaultMaxBytes)
	Timeout      time.Duration         // whole request, body included (<= 0: DefaultTimeout)
	Resolver     Resolver              // nil: net.DefaultResolver
	UserAgent    string
}

// Client fetches URLs under one Policy.
type Client struct {
	policy     Policy
	httpClient *http.Client
}

// New returns a Client enforcing policy. It panics without AllowAddr or
// CheckURL: a fetcher with no rules is never what the caller meant.
func New(policy Policy) *Client {
	if policy.AllowAddr == nil || policy.CheckURL == nil {
		panic("safefetch: Policy.AllowAddr and Policy.CheckURL are required")
	}
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = DefaultMaxBytes
	}
	if policy.Timeout <= 0 {
		policy.Timeout = DefaultTimeout
	}
	if policy.Resolver == nil {
		policy.Resolver = net.DefaultResolver
	}
	client := &Client{policy: policy}
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: tcpKeepAlive, Control: client.control}
	transport := &http.Transport{
		Proxy: nil, // never HTTP(S)_PROXY: the proxy would dial the target, unchecked
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return client.dial(ctx, dialer, network, address)
		},
		TLSHandshakeTimeout:    tlsHandshakeTimeout,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           maxIdleConns,
		IdleConnTimeout:        idleConnTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		DisableCompression:     true, // MaxBytes counts bytes on the wire
	}
	client.httpClient = &http.Client{Transport: transport, CheckRedirect: client.checkRedirect}
	return client
}

// Fetch GETs raw and returns the body (at most MaxBytes) and its declared
// Content-Type, which callers should not trust: sniff the bytes.
func (client *Client) Fetch(ctx context.Context, raw string) ([]byte, string, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("%w: malformed", ErrBlockedURL) // url.Parse errors quote the URL
	}
	if err := client.policy.CheckURL(target); err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrBlockedURL, err)
	}
	ctx, cancel := context.WithTimeout(ctx, client.policy.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrBlockedURL, redact(target))
	}
	if client.policy.UserAgent != "" {
		req.Header.Set("User-Agent", client.policy.UserAgent)
	}
	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, "", wrapErr(ctx, err, target)
	}
	defer func() { _ = resp.Body.Close() }()
	final := redact(resp.Request.URL)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", fmt.Errorf("%w: %d from %s", ErrStatus, resp.StatusCode, final)
	}
	maxBytes := client.policy.MaxBytes
	if resp.ContentLength > maxBytes {
		return nil, "", fmt.Errorf("%w: %s declares %d bytes, max %d", ErrTooLarge, final, resp.ContentLength, maxBytes)
	}
	var body bytes.Buffer
	read, err := body.ReadFrom(io.LimitReader(resp.Body, maxBytes+1)) // one byte past the cap tells "too large" from "exactly the cap"
	switch {
	case err != nil:
		return nil, "", wrapErr(ctx, err, resp.Request.URL)
	case read > maxBytes:
		return nil, "", fmt.Errorf("%w: %s body over %d bytes", ErrTooLarge, final, maxBytes)
	}
	return body.Bytes(), resp.Header.Get("Content-Type"), nil
}

// checkRedirect is http.Client.CheckRedirect: hop count, the caller's URL
// rule, no downgrade, and nothing sensitive carried to the next hop.
func (client *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > client.policy.MaxRedirects {
		return fmt.Errorf("%w: more than %d", ErrTooManyRedirects, client.policy.MaxRedirects)
	}
	if err := client.policy.CheckURL(req.URL); err != nil {
		return fmt.Errorf("%w: redirect to %s: %w", ErrBlockedURL, redact(req.URL), err)
	}
	if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme == "http" {
		return fmt.Errorf("%w: redirect from https to http", ErrBlockedURL)
	}
	for _, header := range []string{"Authorization", "Cookie", "Referer"} {
		req.Header.Del(header)
	}
	return nil
}

// dial resolves the host, refuses the whole name if any answer is refused (a
// mix of public and private answers is a misconfiguration or a rebinding
// attempt), then connects to a vetted IP, never to the name again.
func (client *Client) dial(ctx context.Context, dialer *net.Dialer, network, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, portBase, portBits)
	if err != nil {
		return nil, fmt.Errorf("port %q: %w", portText, err)
	}
	addrs, err := client.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if !client.policy.AllowAddr(addr.Unmap().WithZone("")) {
			return nil, fmt.Errorf("%w: %s is %s", ErrBlockedAddress, host, addr)
		}
	}
	var errs []error
	for _, addr := range addrs {
		conn, err := dialer.DialContext(ctx, network, netip.AddrPortFrom(addr.Unmap(), uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

func (client *Client) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	addrs, err := client.policy.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	return addrs, nil
}

// control is net.Dialer.Control: the last check, on the socket address the
// kernel is about to connect to.
func (client *Client) control(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil || !client.policy.AllowAddr(addrPort.Addr().Unmap().WithZone("")) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
	}
	return nil
}

// wrapErr maps an http.Client error to the package errors, dropping the
// *url.Error wrapper whose message repeats the full URL.
func wrapErr(ctx context.Context, err error, target *url.URL) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}
	var netErr net.Error
	switch {
	case errors.Is(err, ErrBlockedAddress), errors.Is(err, ErrBlockedURL), errors.Is(err, ErrTooManyRedirects):
		return err
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Errorf("%w: %s", ErrTimeout, redact(target))
	default:
		return fmt.Errorf("%w: %s: %w", ErrFetch, redact(target), err)
	}
}

// redact is scheme, host and path only: credentials and signed query
// strings never reach logs.
func redact(target *url.URL) string {
	return target.Scheme + "://" + target.Host + target.EscapedPath()
}
