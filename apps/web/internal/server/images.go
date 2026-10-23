package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/storage"
	"github.com/jayleaton/go-fast-stack/packages/storage/imaging"
	"github.com/jayleaton/go-fast-stack/packages/storage/safefetch"
)

// ObjectStore adapts a *storage.Client to services.ObjectStore.
type ObjectStore struct{ bucket *storage.Client }

var _ services.ObjectStore = ObjectStore{}

// NewObjectStore returns the adapter over bucket.
func NewObjectStore(bucket *storage.Client) ObjectStore { return ObjectStore{bucket: bucket} }

func (store ObjectStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	return store.bucket.Put(ctx, key, data, contentType)
}

func (store ObjectStore) Get(ctx context.Context, key string) (services.ImageFile, error) {
	obj, err := store.bucket.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return services.ImageFile{}, services.ErrImageNotFound
	} else if err != nil {
		return services.ImageFile{}, err
	}
	return services.ImageFile{ContentType: obj.ContentType, Size: obj.Size, Body: obj.Body}, nil
}

func (store ObjectStore) Delete(ctx context.Context, key string) error {
	return store.bucket.Delete(ctx, key)
}

// NewSafeFetch returns the fetch client for user-supplied URLs, enforcing
// the services' outbound rules at dial time and on every redirect.
func NewSafeFetch(userAgent string) *safefetch.Client {
	return safefetch.New(safefetch.Policy{
		AllowAddr:    services.PublicAddr,
		CheckURL:     services.FetchURLAllowed,
		MaxRedirects: services.MaxImageRedirects,
		MaxBytes:     services.MaxImageBytes,
		Timeout:      services.ImageFetchTimeout,
		UserAgent:    userAgent,
	})
}

// ImageFetcher adapts a *safefetch.Client to services.ImageFetcher.
type ImageFetcher struct{ client *safefetch.Client }

var _ services.ImageFetcher = ImageFetcher{}

// NewImageFetcher returns the adapter over client (see NewSafeFetch).
func NewImageFetcher(client *safefetch.Client) ImageFetcher { return ImageFetcher{client: client} }

// Fetch maps the fetch errors to the service's; the declared Content-Type
// is dropped (the service sniffs).
func (fetcher ImageFetcher) Fetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	data, _, err := fetcher.client.Fetch(ctx, rawURL)
	switch {
	case err == nil && int64(len(data)) > maxBytes, errors.Is(err, safefetch.ErrTooLarge):
		return nil, services.ErrImageTooLarge
	case errors.Is(err, safefetch.ErrBlockedAddress), errors.Is(err, safefetch.ErrBlockedURL):
		return nil, fmt.Errorf("%w: %w", services.ErrImageURLNotAllowed, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", services.ErrImageFetchFailed, err)
	}
	return data, nil
}

// ImageCodec adapts an *imaging.Processor to services.ImageCodec.
type ImageCodec struct{ processor *imaging.Processor }

var _ services.ImageCodec = ImageCodec{}

// NewImageCodec returns the adapter over processor.
func NewImageCodec(processor *imaging.Processor) ImageCodec { return ImageCodec{processor: processor} }

func (codec ImageCodec) Normalize(ctx context.Context, data []byte, format services.ImageFormat, maxSide, maxPixels int) (services.NormalizedImage, error) {
	img, err := codec.processor.Normalize(ctx, data, imaging.Format(format), imaging.Options{MaxSide: maxSide, MaxPixels: maxPixels})
	switch {
	case errors.Is(err, imaging.ErrTooManyPixels):
		return services.NormalizedImage{}, services.ErrImageTooManyPixels
	case errors.Is(err, imaging.ErrInvalid), errors.Is(err, imaging.ErrUnsupported):
		return services.NormalizedImage{}, fmt.Errorf("%w: %w", services.ErrImageInvalid, err)
	case err != nil:
		return services.NormalizedImage{}, err
	}
	return services.NormalizedImage{Data: img.Data, Width: img.Width, Height: img.Height}, nil
}
