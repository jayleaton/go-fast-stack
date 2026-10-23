package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// maxErrorBody caps how much of a response body is read: an S3 error
// document, or what is drained before a connection is reused.
const maxErrorBody = 64 << 10 // 64 KiB

// Object is an open object from Get. Stream Body and close it; nothing is
// buffered in memory.
type Object struct {
	ContentType string
	Size        int64 // -1 when the bucket did not say
	Body        io.ReadCloser
}

// Put stores data under key, replacing any object there. S3 verifies the
// body's SHA-256, so a truncated upload fails instead of landing.
func (client *Client) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	req, err := client.request(ctx, http.MethodPut, key, bytes.NewReader(data), hex.EncodeToString(sum[:]), func(header http.Header) {
		header.Set("Content-Type", contentType)
	})
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	return client.do(req)
}

// Get opens key for streaming; ErrNotFound when it does not exist.
func (client *Client) Get(ctx context.Context, key string) (*Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	req, err := client.request(ctx, http.MethodGet, key, nil, emptySHA256, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: get: %s", ErrUnavailable, netClass(err))
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, s3Error(resp)
	}
	return &Object{ContentType: resp.Header.Get("Content-Type"), Size: resp.ContentLength, Body: resp.Body}, nil
}

// Delete removes key; deleting a missing object is not an error.
func (client *Client) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	req, err := client.request(ctx, http.MethodDelete, key, nil, emptySHA256, nil)
	if err != nil {
		return err
	}
	return client.do(req)
}

// Probe makes one cheap signed call (HEAD on the bucket) and reports whether
// the endpoint, bucket and credentials work: nil, ErrAccessDenied (wrong
// keys, no such bucket) or ErrUnavailable.
func (client *Client) Probe(ctx context.Context) error {
	req, err := client.request(ctx, http.MethodHead, "", nil, emptySHA256, nil)
	if err != nil {
		return err
	}
	if err := client.do(req); errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: no such bucket", ErrAccessDenied) // HEAD has no body to carry NoSuchBucket; the name is not logged
	} else if err != nil {
		return err
	}
	return nil
}

func (client *Client) do(req *http.Request) error {
	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %s", ErrUnavailable, strings.ToLower(req.Method), netClass(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return s3Error(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return nil
}

// s3Error maps a failed response to the package errors with its status and
// S3 error code; the body is never quoted.
func s3Error(resp *http.Response) error {
	var s3err struct {
		Code string `xml:"Code"`
	}
	_ = xml.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&s3err)
	what := strings.ToLower(resp.Request.Method) + " " + resp.Status
	if s3err.Code != "" {
		what += " (" + s3err.Code + ")"
	}
	switch {
	case s3err.Code == "NoSuchBucket", resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrAccessDenied, what)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	default:
		return fmt.Errorf("%w: %s", ErrUnavailable, what)
	}
}

// netClass names a transport failure without its message, which carries the
// host (bucket.endpoint), so neither the endpoint nor the bucket reaches logs.
func netClass(err error) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dnsErr):
		return "dns lookup failed"
	case errors.As(err, &opErr):
		return "network error (" + opErr.Op + ")"
	default:
		return "request failed"
	}
}
