package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// request builds a signed request for key (the bucket itself when key is
// ""). headers, when set, adds headers before signing so they are signed too.
func (client *Client) request(ctx context.Context, method, key string, body io.Reader, payloadHash string, headers func(http.Header)) (*http.Request, error) {
	target := *client.base
	path := "/" + key
	if client.cfg.PathStyle {
		path = "/" + client.cfg.Bucket + "/" + key
	} else {
		target.Host = client.cfg.Bucket + "." + client.base.Host
	}
	target.Path, target.RawPath = path, encodePath(path)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("%w: building request: %s", ErrUnavailable, netClass(err))
	}
	if headers != nil {
		headers(req.Header)
	}
	client.sign(req, payloadHash, client.now().UTC())
	return req, nil
}

// sign adds AWS Signature V4 headers (header-based auth). Every header set
// on req before the call is signed, plus host, x-amz-date and
// x-amz-content-sha256. No query strings: this client sends none.
func (client *Client) sign(req *http.Request, payloadHash string, at time.Time) {
	amzDate := at.Format("20060102T150405Z")
	day := at.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	names := []string{"host"}
	canon := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		names = append(names, lower)
		canon[lower] = strings.Join(trimAll(values), ",")
	}
	slices.Sort(names)
	var canonHeaders strings.Builder
	for _, name := range names {
		canonHeaders.WriteString(name + ":" + canon[name] + "\n")
	}
	signed := strings.Join(names, ";")
	canonRequest := strings.Join([]string{req.Method, req.URL.EscapedPath(), "", canonHeaders.String(), signed, payloadHash}, "\n")
	scope := day + "/" + client.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonRequest))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+client.cfg.SecretAccessKey), day)
	key = hmacSHA256(key, client.cfg.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+client.cfg.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
}

func trimAll(values []string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = strings.Join(strings.Fields(value), " ")
	}
	return out
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// encodePath URI-encodes each segment the way SigV4 expects (S3: once):
// unreserved characters stay, everything else is %XX with uppercase hex.
func encodePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		var encoded strings.Builder
		for _, char := range []byte(segment) {
			if ('A' <= char && char <= 'Z') || ('a' <= char && char <= 'z') || ('0' <= char && char <= '9') || char == '-' || char == '_' || char == '.' || char == '~' {
				encoded.WriteByte(char)
			} else {
				fmt.Fprintf(&encoded, "%%%02X", char)
			}
		}
		segments[index] = encoded.String()
	}
	return strings.Join(segments, "/")
}
