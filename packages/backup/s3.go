package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// S3 is a Store on any S3-compatible bucket (Railway storage buckets, AWS S3,
// Cloudflare R2, Backblaze B2, MinIO) over plain net/http with AWS
// Signature V4: the four calls a backup needs, no SDK.
type S3 struct {
	Endpoint        string // https://t3.storageapi.dev (Railway), https://<account>.r2.cloudflarestorage.com, http://127.0.0.1:9000
	Bucket          string
	Region          string // "auto" for Railway and R2; "us-east-1" for MinIO
	AccessKeyID     string
	SecretAccessKey string
	// PathStyle addresses the bucket as endpoint/bucket/key (MinIO, old
	// buckets); otherwise bucket.endpoint/key (virtual-hosted, the default
	// on Railway, R2 and AWS).
	PathStyle  bool
	HTTPClient *http.Client // default: connect, TLS and response-header timeouts, none on the body (a restore streams for as long as it takes)
	now        func() time.Time
}

var _ Store = (*S3)(nil)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

const (
	s3TLSHandshakeTimeout = 15 * time.Second // the default HTTPClient's TLS handshake timeout
	maxS3ListPage         = 16 << 20         // 16 MiB cap on one ListObjectsV2 response
	maxS3ErrorBody        = 64 << 10         // 64 KiB of an error response read for its code
)

// Put uploads size bytes from body. sha256hex is the body's SHA-256: S3
// verifies it, so a truncated upload fails instead of landing.
func (s *S3) Put(ctx context.Context, key string, body io.Reader, size int64, sha256hex string) error {
	req, err := s.request(ctx, http.MethodPut, key, nil, body, sha256hex)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	return s.do(req, nil)
}

// Get opens key for reading.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := s.request(ctx, http.MethodGet, key, nil, nil, emptySHA256)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("backup: s3 get: %w", redactURL(err))
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, s3Error(resp)
	}
	return resp.Body, nil
}

// Delete removes key (no error when it is already gone).
func (s *S3) Delete(ctx context.Context, key string) error {
	req, err := s.request(ctx, http.MethodDelete, key, nil, nil, emptySHA256)
	if err != nil {
		return err
	}
	return s.do(req, nil)
}

// List returns every object under prefix (ListObjectsV2, all pages).
func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := s.request(ctx, http.MethodGet, "", q, nil, emptySHA256)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := s.do(req, &page); err != nil {
			return nil, err
		}
		for _, c := range page.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, Modified: c.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

func (s *S3) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = time.Minute
	t.TLSHandshakeTimeout = s3TLSHandshakeTimeout
	return &http.Client{Transport: t}
}

func (s *S3) do(req *http.Request, into any) error {
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("backup: s3 %s: %w", strings.ToLower(req.Method), redactURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return s3Error(resp)
	}
	if into == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxS3ListPage)).Decode(into); err != nil {
		return fmt.Errorf("backup: s3 list: malformed response: %w", err)
	}
	return nil
}

// s3Error reports the status and S3 error code; the message can echo the
// request but never credentials, which are not in it (only the signature).
func s3Error(resp *http.Response) error {
	var e struct {
		Code string `xml:"Code"`
	}
	_ = xml.NewDecoder(io.LimitReader(resp.Body, maxS3ErrorBody)).Decode(&e)
	err := fmt.Errorf("backup: s3 %s %s", strings.ToLower(resp.Request.Method), resp.Status)
	if e.Code != "" {
		err = fmt.Errorf("%w (%s)", err, e.Code)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

func redactURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// request builds a signed request for key (or the bucket when key is "").
func (s *S3) request(ctx context.Context, method, key string, query url.Values, body io.Reader, payloadHash string) (*http.Request, error) {
	base, err := url.Parse(strings.TrimRight(s.Endpoint, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("backup: S3 endpoint must be an http(s) URL")
	}
	if s.Bucket == "" || s.AccessKeyID == "" || s.SecretAccessKey == "" {
		return nil, errors.New("backup: S3 bucket and credentials are required")
	}
	u := *base
	path := "/" + key
	if s.PathStyle {
		path = "/" + s.Bucket + "/" + key
	} else {
		u.Host = s.Bucket + "." + base.Host
	}
	if key == "" && !s.PathStyle {
		path = "/"
	}
	u.Path, u.RawPath = path, encodePath(path)
	u.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("backup: s3 request: %w", err)
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	s.sign(req, payloadHash, now().UTC())
	return req, nil
}

// sign adds AWS Signature V4 headers (header-based auth). Every header set
// on req before the call is signed, plus host, x-amz-date and
// x-amz-content-sha256.
func (s *S3) sign(req *http.Request, payloadHash string, t time.Time) {
	region := s.Region
	if region == "" {
		region = "auto"
	}
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	names := []string{"host"}
	canon := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		names = append(names, lk)
		canon[lk] = strings.Join(trimAll(v), ",")
	}
	slices.Sort(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + canon[n] + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, ch.String(), signed, payloadHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(creq))

	k := hmacSHA256([]byte("AWS4"+s.SecretAccessKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, sts))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func trimAll(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.Join(strings.Fields(s), " ")
	}
	return out
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// encodePath URI-encodes each segment the way SigV4 expects (S3: once).
func encodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		vs := slices.Clone(q[k])
		slices.Sort(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's UriEncode: unreserved characters stay, everything
// else is %XX with uppercase hex.
func uriEncode(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
