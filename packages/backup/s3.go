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
func (bucket *S3) Put(ctx context.Context, key string, body io.Reader, size int64, sha256hex string) error {
	req, err := bucket.request(ctx, http.MethodPut, key, nil, body, sha256hex)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	return bucket.do(req, nil)
}

// Get opens key for reading.
func (bucket *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := bucket.request(ctx, http.MethodGet, key, nil, nil, emptySHA256)
	if err != nil {
		return nil, err
	}
	resp, err := bucket.client().Do(req)
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
func (bucket *S3) Delete(ctx context.Context, key string) error {
	req, err := bucket.request(ctx, http.MethodDelete, key, nil, nil, emptySHA256)
	if err != nil {
		return err
	}
	return bucket.do(req, nil)
}

// List returns every object under prefix (ListObjectsV2, all pages).
func (bucket *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		values := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			values.Set("continuation-token", token)
		}
		req, err := bucket.request(ctx, http.MethodGet, "", values, nil, emptySHA256)
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
		if err := bucket.do(req, &page); err != nil {
			return nil, err
		}
		for _, content := range page.Contents {
			out = append(out, Object{Key: content.Key, Size: content.Size, Modified: content.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

func (bucket *S3) client() *http.Client {
	if bucket.HTTPClient != nil {
		return bucket.HTTPClient
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Minute
	transport.TLSHandshakeTimeout = s3TLSHandshakeTimeout
	return &http.Client{Transport: transport}
}

func (bucket *S3) do(req *http.Request, into any) error {
	resp, err := bucket.client().Do(req)
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
	var item struct {
		Code string `xml:"Code"`
	}
	_ = xml.NewDecoder(io.LimitReader(resp.Body, maxS3ErrorBody)).Decode(&item)
	err := fmt.Errorf("backup: s3 %s %s", strings.ToLower(resp.Request.Method), resp.Status)
	if item.Code != "" {
		err = fmt.Errorf("%w (%s)", err, item.Code)
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
func (bucket *S3) request(ctx context.Context, method, key string, query url.Values, body io.Reader, payloadHash string) (*http.Request, error) {
	base, err := url.Parse(strings.TrimRight(bucket.Endpoint, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("backup: S3 endpoint must be an http(s) URL")
	}
	if bucket.Bucket == "" || bucket.AccessKeyID == "" || bucket.SecretAccessKey == "" {
		return nil, errors.New("backup: S3 bucket and credentials are required")
	}
	link := *base
	path := "/" + key
	if bucket.PathStyle {
		path = "/" + bucket.Bucket + "/" + key
	} else {
		link.Host = bucket.Bucket + "." + base.Host
	}
	if key == "" && !bucket.PathStyle {
		path = "/"
	}
	link.Path, link.RawPath = path, encodePath(path)
	link.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, link.String(), body)
	if err != nil {
		return nil, fmt.Errorf("backup: s3 request: %w", err)
	}
	now := time.Now
	if bucket.now != nil {
		now = bucket.now
	}
	bucket.sign(req, payloadHash, now().UTC())
	return req, nil
}

// sign adds AWS Signature V4 headers (header-based auth). Every header set
// on req before the call is signed, plus host, x-amz-date and
// x-amz-content-sha256.
func (bucket *S3) sign(req *http.Request, payloadHash string, moment time.Time) {
	region := bucket.Region
	if region == "" {
		region = "auto"
	}
	amzDate := moment.Format("20060102T150405Z")
	day := moment.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	names := []string{"host"}
	canon := map[string]string{"host": req.URL.Host}
	for key, values := range req.Header {
		lk := strings.ToLower(key)
		names = append(names, lk)
		canon[lk] = strings.Join(trimAll(values), ",")
	}
	slices.Sort(names)
	var ch strings.Builder
	for _, name := range names {
		ch.WriteString(name + ":" + canon[name] + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, ch.String(), signed, payloadHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(creq))

	signingKey := hmacSHA256([]byte("AWS4"+bucket.SecretAccessKey), day)
	signingKey = hmacSHA256(signingKey, region)
	signingKey = hmacSHA256(signingKey, "s3")
	signingKey = hmacSHA256(signingKey, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(signingKey, sts))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+bucket.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func trimAll(values []string) []string {
	out := make([]string, len(values))
	for index, text := range values {
		out[index] = strings.Join(strings.Fields(text), " ")
	}
	return out
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// encodePath URI-encodes each segment the way SigV4 expects (S3: once).
func encodePath(path string) string {
	segs := strings.Split(path, "/")
	for index, segment := range segs {
		segs[index] = uriEncode(segment)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var parts []string
	for _, key := range keys {
		vs := slices.Clone(values[key])
		slices.Sort(vs)
		for _, value := range vs {
			parts = append(parts, uriEncode(key)+"="+uriEncode(value))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's UriEncode: unreserved characters stay, everything
// else is %XX with uppercase hex.
func uriEncode(text string) string {
	var builder strings.Builder
	for index := range len(text) {
		char := text[index]
		if ('A' <= char && char <= 'Z') || ('a' <= char && char <= 'z') || ('0' <= char && char <= '9') || char == '-' || char == '_' || char == '.' || char == '~' {
			builder.WriteByte(char)
		} else {
			fmt.Fprintf(&builder, "%%%02X", char)
		}
	}
	return builder.String()
}
