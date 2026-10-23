package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/webp"
)

// Magic bytes and header sizes the format check reads.
const (
	riffHeaderLen = 12 // "RIFF" + 4-byte size + "WEBP"
	ftypBoxLen    = 12 // 4-byte size + "ftyp" + major brand
)

// magic reports whether data starts like format, so garbage is refused
// before any decoder (or the AVIF wasm runtime) runs.
func magic(data []byte, format Format) bool {
	switch format {
	case JPEG:
		return bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff})
	case PNG:
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case GIF:
		return bytes.HasPrefix(data, []byte("GIF8"))
	case WebP:
		return len(data) >= riffHeaderLen && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	case AVIF:
		return len(data) >= ftypBoxLen && string(data[4:8]) == "ftyp"
	}
	return false
}

// checkSize reads only the header of data and refuses anything over
// maxPixels, before a decoder allocates the full image.
func checkSize(data []byte, format Format, maxPixels int) error {
	var decodeConfig func(io.Reader) (image.Config, error)
	switch format {
	case JPEG:
		decodeConfig = jpeg.DecodeConfig
	case PNG:
		decodeConfig = png.DecodeConfig
	case GIF:
		decodeConfig = gif.DecodeConfig
	case WebP:
		decodeConfig = webp.DecodeConfig
	case AVIF:
		decodeConfig = avif.DecodeConfig
	default:
		return ErrUnsupported
	}
	if !magic(data, format) {
		return fmt.Errorf("%w (%s): not a %s file", ErrInvalid, format, format)
	}
	header, err := decodeConfig(bytes.NewReader(data))
	if err != nil {
		return decodeErr(format, err)
	}
	if header.Width <= 0 || header.Height <= 0 {
		return fmt.Errorf("%w (%s): empty image", ErrInvalid, format)
	}
	if header.Width > maxPixels/header.Height {
		return fmt.Errorf("%w: %dx%d is over %d", ErrTooManyPixels, header.Width, header.Height, maxPixels)
	}
	return nil
}

// decode returns the first frame of data and the EXIF orientation (1-8)
// still to apply to it. AVIF comes back already oriented (irot/imir).
func decode(data []byte, format Format) (image.Image, int, error) {
	reader := bytes.NewReader(data)
	var (
		img         image.Image
		err         error
		orientation = orientUpright
	)
	switch format {
	case JPEG:
		img, err = jpeg.Decode(reader)
		orientation = tiffOrientation(jpegExif(data))
	case PNG:
		img, err = png.Decode(reader)
		orientation = tiffOrientation(pngExif(data))
	case GIF:
		img, err = gif.Decode(reader) // stops after the first frame
	case WebP:
		img, err = webp.Decode(reader) // no AutoRotate: its fallback is per-pixel; orient does it on RGBA
		orientation = tiffOrientation(webpExif(data))
	case AVIF:
		img, err = avif.Decode(reader, avif.Options{AutoRotate: true})
	default:
		return nil, 0, ErrUnsupported
	}
	if err != nil {
		return nil, 0, decodeErr(format, err)
	}
	return img, orientation, nil
}

// decodeErr maps a decoder error to ErrUnsupported (a valid file using a
// feature the decoder lacks) or ErrInvalid.
func decodeErr(format Format, err error) error {
	var jpegErr jpeg.UnsupportedError
	var pngErr png.UnsupportedError
	if errors.As(err, &jpegErr) || errors.As(err, &pngErr) {
		return fmt.Errorf("%w (%s): %w", ErrUnsupported, format, err)
	}
	return fmt.Errorf("%w (%s): %w", ErrInvalid, format, err)
}
