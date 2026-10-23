// Package imaging turns an untrusted upload (JPEG, PNG, GIF, WebP or AVIF)
// into a clean WebP: upright (EXIF/irot orientation applied), metadata
// stripped, fitted inside a bounding box. It is pure Go (libwebp transpiled to
// Go, libavif under wazero), so the server binary stays static; build it with
// -tags nodynamic so the codecs never try to dlopen a system libwebp/libavif.
//
// One Processor per process bounds how many images are decoded at once:
//
//	processor := imaging.New(2) // at most two decodes at once
//	out, err := processor.Normalize(ctx, body, imaging.JPEG, imaging.Options{
//		MaxPixels: 50_000_000, // refuse decompression bombs from the header
//		MaxSide:   2048,
//	})
//	switch {
//	case errors.Is(err, imaging.ErrTooManyPixels), errors.Is(err, imaging.ErrInvalid),
//		errors.Is(err, imaging.ErrUnsupported):
//		// 4xx: the upload is the problem
//	case err != nil:
//		// ctx cancelled while queued
//	}
//	// out.Data is image/webp, out.Width x out.Height
package imaging
