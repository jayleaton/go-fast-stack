package services

//go:generate go tool mockgen -source=images.go -destination=mocks/images.go -package=mocks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/url"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// ImageService is the optional-storage example: a signed-in user uploads an
// image (or names a URL to import one from), the service checks and
// normalises it to WebP, and stores it in the bucket under the user's own
// prefix. It exists only when object storage is configured (README
// "Object storage"); delete this file, its handlers and the wiring to drop it.

// ImageFormat is a format the service accepts, sniffed from the bytes.
type ImageFormat string

const (
	FormatJPEG ImageFormat = "jpeg"
	FormatPNG  ImageFormat = "png"
	FormatGIF  ImageFormat = "gif"
	FormatWebP ImageFormat = "webp"
	FormatAVIF ImageFormat = "avif"
)

// imageIDBytes is the random part of an image id (128 bits).
const imageIDBytes = 16

// Image limits. Uploads and imports share them.
const (
	MaxImageBytes     = 10 << 20         // encoded input
	MaxImagePixels    = 50_000_000       // decoded input (a 48 MP phone photo fits); refused from the header, before decoding
	MaxImageSide      = 2048             // output fits inside MaxImageSide x MaxImageSide
	MaxImageURLLength = 2048             // Import URL, and every redirect target
	MaxImageRedirects = 3                // Import redirects followed
	ImageFetchTimeout = 20 * time.Second // whole Import fetch, body included
)

// Image is a stored image as clients see it.
type Image struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Size        int    `json:"size"` // bytes
}

// ImageFile is a stored image opened for streaming; the caller closes Body.
type ImageFile struct {
	ContentType string
	Size        int64 // -1 when unknown
	Body        io.ReadCloser
}

// NormalizedImage is what ImageCodec.Normalize returns: WebP bytes.
type NormalizedImage struct {
	Data          []byte
	Width, Height int
}

// ObjectStore is the port to the bucket (an adapter over packages/storage).
// Get fails with ErrImageNotFound for a missing key; deleting a missing key
// is not an error.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) (ImageFile, error)
	Delete(ctx context.Context, key string) error
}

// ImageFetcher GETs a user-supplied URL (an adapter over
// packages/storage/safefetch). It must dial only addresses PublicAddr allows,
// checked at dial time, request only URLs FetchURLAllowed allows (redirects
// included, at most MaxImageRedirects), and read at most maxBytes before
// ctx's deadline. It fails with ErrImageURLNotAllowed, ErrImageTooLarge or
// ErrImageFetchFailed.
type ImageFetcher interface {
	Fetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error)
}

// ImageCodec decodes data as format, applies its orientation, fits it inside
// maxSide x maxSide (never enlarging), strips metadata and encodes WebP (an
// adapter over packages/storage/imaging). It fails with
// ErrImageTooManyPixels (checked before decoding) or ErrImageInvalid.
type ImageCodec interface {
	Normalize(ctx context.Context, data []byte, format ImageFormat, maxSide, maxPixels int) (NormalizedImage, error)
}

// ImageService errors.
var (
	ErrImageNotFound      = &Error{KindNotFound, "Image not found"}
	ErrImageTooLarge      = &Error{KindInvalid, "Image is larger than 10 MB"}
	ErrImageTooManyPixels = &Error{KindInvalid, "Image has too many pixels"}
	ErrImageUnsupported   = &Error{KindInvalid, "Unsupported image type: use JPEG, PNG, GIF, WebP or AVIF"}
	ErrImageInvalid       = &Error{KindInvalid, "Image could not be read"}
	ErrImageURLNotAllowed = &Error{KindInvalid, "This URL cannot be fetched"}
	ErrImageFetchFailed   = &Error{KindInvalid, "Image could not be fetched from the URL"}
)

// ImageService stores users' images.
type ImageService struct {
	store ObjectStore
	fetch ImageFetcher
	codec ImageCodec
}

// NewImageService returns the service over its ports.
func NewImageService(store ObjectStore, fetch ImageFetcher, codec ImageCodec) *ImageService {
	return &ImageService{store: store, fetch: fetch, codec: codec}
}

// Upload stores an uploaded image for the actor.
func (service *ImageService) Upload(ctx context.Context, actor Actor, data []byte) (Image, error) {
	if actor.UserID == "" {
		return Image{}, ErrForbidden
	}
	return service.ingest(ctx, actor, data)
}

// Import fetches in.URL server-side (see ImageFetcher for the rules) and
// stores the image for the actor.
func (service *ImageService) Import(ctx context.Context, actor Actor, in validate.CreateImageFromURL) (Image, error) {
	if actor.UserID == "" {
		return Image{}, ErrForbidden
	}
	target, err := url.Parse(in.URL)
	if err != nil {
		return Image{}, ErrImageURLNotAllowed
	}
	if err := FetchURLAllowed(target); err != nil {
		return Image{}, err
	}
	fctx, cancel := context.WithTimeout(ctx, ImageFetchTimeout)
	defer cancel()
	data, err := service.fetch.Fetch(fctx, in.URL, MaxImageBytes)
	if err != nil {
		return Image{}, err
	}
	return service.ingest(ctx, actor, data)
}

// Open opens one of the actor's images. Another user's id is not found.
func (service *ImageService) Open(ctx context.Context, actor Actor, id string) (ImageFile, error) {
	key, err := imageKey(actor, id)
	if err != nil {
		return ImageFile{}, err
	}
	return service.store.Get(ctx, key)
}

// Delete removes one of the actor's images (no error if it is already gone).
func (service *ImageService) Delete(ctx context.Context, actor Actor, id string) error {
	key, err := imageKey(actor, id)
	if err != nil {
		return err
	}
	return service.store.Delete(ctx, key)
}

// ingest is the shared rule: size, sniffed format (the declared type is
// never trusted), normalise, store under a new random id.
func (service *ImageService) ingest(ctx context.Context, actor Actor, data []byte) (Image, error) {
	if len(data) > MaxImageBytes {
		return Image{}, ErrImageTooLarge
	}
	format, ok := SniffImage(data)
	if !ok {
		return Image{}, ErrImageUnsupported
	}
	img, err := service.codec.Normalize(ctx, data, format, MaxImageSide, MaxImagePixels)
	if err != nil {
		return Image{}, err
	}
	id := newImageID()
	key, _ := imageKey(actor, id)
	if err := service.store.Put(ctx, key, img.Data, "image/webp"); err != nil {
		return Image{}, err
	}
	return Image{ID: id, ContentType: "image/webp", Width: img.Width, Height: img.Height, Size: len(img.Data)}, nil
}

// imageKey is where the actor's image id lives. Keys are namespaced by the
// actor, so no id reaches another user's objects; an id that cannot exist is
// not found (and never reaches the bucket).
func imageKey(actor Actor, id string) (string, error) {
	if actor.UserID == "" {
		return "", ErrForbidden
	}
	if len(id) != imageIDBytes*2 || !isLowerHex(id) { // hex: two characters per byte
		return "", ErrImageNotFound
	}
	return "users/" + actor.UserID + "/images/" + id + ".webp", nil
}

func newImageID() string {
	id := make([]byte, imageIDBytes)
	_, _ = rand.Read(id) // never fails (crypto/rand)
	return hex.EncodeToString(id)
}

func isLowerHex(text string) bool {
	for _, char := range []byte(text) {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// SniffImage names the format of data from its magic bytes, or false.
// HEIC and other ISO-BMFF images that are not AVIF are refused.
func SniffImage(data []byte) (ImageFormat, bool) {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return FormatJPEG, true
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return FormatPNG, true
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return FormatGIF, true
	case len(data) >= riffHeaderLen && string(data[:4]) == "RIFF" && string(data[8:riffHeaderLen]) == "WEBP":
		return FormatWebP, true
	case isAVIF(data):
		return FormatAVIF, true
	}
	return "", false
}

// Container layout SniffImage reads.
const (
	riffHeaderLen    = 12   // "RIFF" + 4-byte size + "WEBP"
	ftypMinLen       = 16   // 4-byte size + "ftyp" + major brand + minor version
	ftypMaxLen       = 4096 // a ftyp box is a few brands; anything bigger is not one we read
	ftypBrandsStart  = 8    // the major brand, then the minor version, then compatible brands
	ftypMinorVersion = 12   // offset of the minor version, which is not a brand
	brandLen         = 4
)

// isAVIF reads the leading ftyp box: the major brand or a compatible brand
// must be avif (still) or avis (sequence).
func isAVIF(data []byte) bool {
	if len(data) < ftypMinLen || string(data[4:8]) != "ftyp" {
		return false
	}
	size := int(binary.BigEndian.Uint32(data))
	if size < ftypMinLen || size > len(data) || size > ftypMaxLen {
		return false
	}
	for offset := ftypBrandsStart; offset+brandLen <= size; offset += brandLen {
		if offset == ftypMinorVersion {
			continue
		}
		if brand := string(data[offset : offset+brandLen]); brand == "avif" || brand == "avis" {
			return true
		}
	}
	return false
}
