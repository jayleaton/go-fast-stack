package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// Object storage (README "Object storage") is optional: off unless
// STORAGE_S3_BUCKET is set. Which bucket and which keys are live secrets, so
// a bucket move or a key rotation needs no redeploy; the region and
// addressing style are plain env.
const (
	secretStorageEndpoint  = "STORAGE_S3_ENDPOINT"
	secretStorageBucket    = "STORAGE_S3_BUCKET"
	secretStorageAccessKey = "STORAGE_S3_ACCESS_KEY_ID"
	secretStorageSecretKey = "STORAGE_S3_SECRET_ACCESS_KEY"
)

var storageSecrets = []string{secretStorageEndpoint, secretStorageBucket, secretStorageAccessKey, secretStorageSecretKey}

// StorageOptions are the non-secret storage settings.
type StorageOptions struct {
	Region    string // STORAGE_S3_REGION: "" is "auto" (Railway, R2); AWS needs the bucket's region
	PathStyle bool   // STORAGE_S3_PATH_STYLE: endpoint/bucket/key addressing (MinIO)
}

// StorageCredentials say which bucket to use and how to sign in to it. All
// four are set, or none (storage off).
type StorageCredentials struct {
	Endpoint, Bucket, AccessKeyID, SecretAccessKey secrets.Secret
}

// Configured reports whether object storage is on.
func (credentials StorageCredentials) Configured() bool { return !credentials.Bucket.IsZero() }

// Fingerprint names the set in logs without revealing it.
func (credentials StorageCredentials) Fingerprint() string {
	return "endpoint:" + credentials.Endpoint.Fingerprint() + ",bucket:" + credentials.Bucket.Fingerprint() +
		",key:" + credentials.AccessKeyID.Fingerprint() + ",secret:" + credentials.SecretAccessKey.Fingerprint()
}

// Storage returns the current storage credentials (after Start), trimmed.
func (store Secrets) Storage() (StorageCredentials, error) {
	values := make(map[string]secrets.Secret, len(storageSecrets))
	for _, name := range storageSecrets {
		value, err := store.m.Get(name)
		if err != nil {
			return StorageCredentials{}, err
		}
		values[name] = value.TrimSpace()
	}
	return StorageCredentials{
		Endpoint:        values[secretStorageEndpoint],
		Bucket:          values[secretStorageBucket],
		AccessKeyID:     values[secretStorageAccessKey],
		SecretAccessKey: values[secretStorageSecretKey],
	}, nil
}

func loadStorageOptions(get func(key, fallback string) string, errs *[]error) StorageOptions {
	raw := get("STORAGE_S3_PATH_STYLE", "false")
	pathStyle, err := strconv.ParseBool(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("STORAGE_S3_PATH_STYLE must be true or false, got %q", raw))
	}
	return StorageOptions{Region: get("STORAGE_S3_REGION", ""), PathStyle: pathStyle}
}

// validateStorage requires all four storage secrets or none, and an http(s)
// endpoint. Messages name keys, never values.
func validateStorage(raw func(string) string) []error {
	get := func(name string) string { return strings.TrimSpace(raw(name)) }
	set := 0
	for _, name := range storageSecrets {
		if get(name) != "" {
			set++
		}
	}
	if set == 0 {
		return nil
	}
	var errs []error
	for _, name := range storageSecrets {
		if get(name) == "" {
			errs = append(errs, fmt.Errorf("%s is required when any STORAGE_S3_* secret is set", name))
		}
	}
	if endpoint := get(secretStorageEndpoint); endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			errs = append(errs, errors.New("STORAGE_S3_ENDPOINT must be an http(s) URL without credentials"))
		}
	}
	return errs
}
