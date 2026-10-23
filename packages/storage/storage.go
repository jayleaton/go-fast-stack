// Package storage keeps binary objects (images, uploads) under string keys in
// any S3-compatible bucket: Railway storage buckets, Cloudflare R2, AWS S3,
// Backblaze B2, MinIO. It speaks the S3 API over plain net/http with AWS
// Signature V4: the few calls an app needs, no SDK.
//
//	bucket, err := storage.New(storage.Config{Endpoint: ..., Bucket: ..., AccessKeyID: ..., SecretAccessKey: ...})
//	err = bucket.Put(ctx, "users/u1/images/a.webp", data, "image/webp")
//	obj, err := bucket.Get(ctx, "users/u1/images/a.webp") // stream obj.Body, then Close
//
// Objects are private: they have no public URL and are served through the
// app's own authenticated routes. Services declare the port they need and an
// adapter in apps/*/internal/server wraps a Client; credentials arrive as
// plain strings revealed in cmd. This package never reads the env or
// packages/secrets.
//
// Subpackages: imaging decodes and normalises untrusted images (pure Go),
// safefetch fetches untrusted URLs without reaching private networks.
package storage

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Errors every Client method wraps at most one of; match with errors.Is.
// Messages never contain credentials or provider response bodies.
var (
	// ErrNotFound: the object does not exist (S3 404 / NoSuchKey).
	ErrNotFound = errors.New("storage: object not found")
	// ErrAccessDenied: the bucket refused the credentials or the operation
	// (S3 403), or the bucket does not exist (NoSuchBucket).
	ErrAccessDenied = errors.New("storage: access denied")
	// ErrUnavailable: any other failure (network, 5xx, malformed response).
	ErrUnavailable = errors.New("storage: unavailable")
	// ErrInvalidKey: the key is empty, too long, or could escape a directory
	// tree ("..", ".", empty segments, a leading "/", backslash or NUL).
	ErrInvalidKey = errors.New("storage: invalid object key")
)

// DefaultRegion is the SigV4 region Railway and R2 expect.
const DefaultRegion = "auto"

const (
	responseHeaderTimeout = time.Minute      // default client: a bucket that accepts a request but never answers
	tlsHandshakeTimeout   = 15 * time.Second // default client
	maxKeyLength          = 1024             // bytes; S3's own limit on object keys
)

// Config names one bucket and the credentials for it.
type Config struct {
	Endpoint        string // https://t3.storageapi.dev (Railway), https://<account>.r2.cloudflarestorage.com, http://127.0.0.1:9000
	Bucket          string
	Region          string // "" is DefaultRegion; AWS needs the bucket's region
	AccessKeyID     string
	SecretAccessKey string
	// PathStyle addresses the bucket as endpoint/bucket/key (MinIO);
	// otherwise bucket.endpoint/key (virtual-hosted: Railway, R2, AWS).
	PathStyle bool
	// HTTPClient overrides the default client (connect, TLS and
	// response-header timeouts; none on the body, which streams).
	HTTPClient *http.Client
}

// String keeps the endpoint, bucket and credentials (an app's live secrets)
// out of logs and %v.
func (config Config) String() string {
	return fmt.Sprintf("storage.Config{Endpoint:[redacted] Bucket:[redacted] Region:%s PathStyle:%t AccessKeyID:[redacted] SecretAccessKey:[redacted]}",
		config.Region, config.PathStyle)
}

// GoString keeps the credentials out of %#v.
func (config Config) GoString() string { return config.String() }

// Client is one bucket. It is safe for concurrent use; build it with New.
type Client struct {
	base       *url.URL
	cfg        Config
	httpClient *http.Client
	now        func() time.Time
}

// New checks cfg and returns a Client. It does no I/O: call Probe to find
// out whether the bucket and credentials work.
func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil {
		return nil, errors.New("storage: endpoint must be an http(s) URL without credentials")
	}
	if cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("storage: bucket and credentials are required")
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.TLSHandshakeTimeout = tlsHandshakeTimeout
		httpClient = &http.Client{Transport: transport}
	}
	return &Client{base: base, cfg: cfg, httpClient: httpClient, now: time.Now}, nil
}

// validateKey rejects keys that are empty or could escape a directory tree
// (another store, a download name). It never rewrites a key.
func validateKey(key string) error {
	if key == "" || len(key) > maxKeyLength || strings.ContainsAny(key, "\\\x00") {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	for segment := range strings.SplitSeq(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
	}
	return nil
}
