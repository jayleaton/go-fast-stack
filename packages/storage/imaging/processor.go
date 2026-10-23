package imaging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"

	"github.com/gen2brain/webp"
)

// Format is the declared format of an input; Normalize refuses bytes that
// are not that format.
type Format string

const (
	JPEG Format = "jpeg"
	PNG  Format = "png"
	GIF  Format = "gif"
	WebP Format = "webp"
	AVIF Format = "avif"
)

// Errors returned by Normalize; compare with errors.Is. The messages carry
// no image bytes and are safe to log.
var (
	ErrUnsupported   = errors.New("imaging: unsupported image format")
	ErrInvalid       = errors.New("imaging: invalid or corrupt image")
	ErrTooManyPixels = errors.New("imaging: image has too many pixels")
)

// Options bound what Normalize accepts and produces.
type Options struct {
	MaxPixels int // reject inputs over width*height pixels, read from the header before decoding (0: 50 MP)
	MaxSide   int // fit the output inside MaxSide x MaxSide keeping aspect, never enlarging (0 or over 16383: 16383, WebP's limit)
	Quality   int // WebP quality 1..100 (0: 82)
}

// Image is a normalised image: WebP bytes, upright, metadata stripped,
// 8-bit, alpha kept only when the input has translucent pixels.
type Image struct {
	Data          []byte
	Width, Height int
}

const (
	defaultMaxPixels   = 50_000_000 // a 48 MP phone photo fits
	maxWebPSide        = 16383      // the WebP format's limit per side
	defaultQuality     = 82         // libwebp quality; Sharp's default for photos
	maxQuality         = 100
	defaultConcurrency = 2    // two decodes at once; one 48 MP photo peaks at about 70 MB
	bytesPerRGBAPixel  = 4    // R, G, B, A
	alphaOpaque        = 0xff // 8-bit alpha of an opaque pixel
	alphaChannel       = 3    // offset of A in an RGBA pixel
)

// Processor normalises images, at most a fixed number at once: a decoded
// 48 MP photo is about 70 MB (73 MB of YCbCr), so the slot count bounds memory.
type Processor struct {
	slots chan struct{}
}

// New returns a Processor running at most concurrency normalisations at once
// (<= 0: defaultConcurrency). Callers beyond that queue for a slot.
func New(concurrency int) *Processor {
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	return &Processor{slots: make(chan struct{}, concurrency)}
}

// Normalize decodes data as format, applies its orientation, fits it inside
// options.MaxSide and encodes WebP. Oversized inputs are refused from the
// header before taking a slot; ctx cancels the wait for a slot.
func (processor *Processor) Normalize(ctx context.Context, data []byte, format Format, options Options) (img Image, err error) {
	defer func() {
		if recover() != nil { // a third-party decoder choked on hostile input
			img, err = Image{}, fmt.Errorf("%w (%s): decoder panic", ErrInvalid, format)
		}
	}()
	if options.MaxPixels <= 0 {
		options.MaxPixels = defaultMaxPixels
	}
	if err := checkSize(data, format, options.MaxPixels); err != nil {
		return Image{}, err
	}

	select {
	case processor.slots <- struct{}{}:
		defer func() { <-processor.slots }()
	case <-ctx.Done():
		return Image{}, ctx.Err()
	}

	src, orientation, err := decode(data, format)
	if err != nil {
		return Image{}, err
	}
	rgba := orient(fit(src, options.MaxSide), orientation)
	return encode(rgba, options.Quality)
}

// fit converts src to a fresh *image.RGBA at the origin, scaled down with
// Catmull-Rom when a side is over maxSide.
func fit(src image.Image, maxSide int) *image.RGBA {
	if maxSide <= 0 || maxSide > maxWebPSide {
		maxSide = maxWebPSide
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= maxSide && height <= maxSide {
		if rgba, ok := src.(*image.RGBA); ok && bounds.Min == (image.Point{}) && rgba.Stride == bytesPerRGBAPixel*width {
			return rgba
		}
		dst := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
		return dst
	}
	// The long side becomes maxSide; the short side keeps the aspect, rounded.
	newWidth, newHeight := maxSide, max(1, (height*maxSide+width/2)/width)
	if height > width {
		newWidth, newHeight = max(1, (width*maxSide+height/2)/height), maxSide
	}
	return resize(src, newWidth, newHeight)
}

// encode writes img as WebP. libwebp reads straight (non-premultiplied)
// alpha, so translucent images are un-premultiplied in place first; opaque
// ones encode without an alpha channel.
func encode(img *image.RGBA, quality int) (Image, error) {
	if quality <= 0 || quality > maxQuality {
		quality = defaultQuality
	}
	var straight image.Image = img
	if !img.Opaque() {
		straight = unpremultiply(img)
	}
	var out bytes.Buffer
	if err := webp.Encode(&out, straight, webp.Options{Quality: quality, Method: webp.DefaultMethod}); err != nil {
		return Image{}, fmt.Errorf("imaging: encode webp: %w", err)
	}
	return Image{Data: out.Bytes(), Width: img.Rect.Dx(), Height: img.Rect.Dy()}, nil
}

// unpremultiply reuses img's pixels as non-premultiplied NRGBA.
func unpremultiply(img *image.RGBA) *image.NRGBA {
	pix := img.Pix
	for pixel := 0; pixel+alphaChannel < len(pix); pixel += bytesPerRGBAPixel {
		if alpha := uint32(pix[pixel+alphaChannel]); alpha != 0 && alpha != alphaOpaque {
			for channel := pixel; channel < pixel+alphaChannel; channel++ {
				pix[channel] = uint8(min((uint32(pix[channel])*alphaOpaque+alpha/2)/alpha, alphaOpaque))
			}
		}
	}
	return &image.NRGBA{Pix: pix, Stride: img.Stride, Rect: img.Rect}
}
