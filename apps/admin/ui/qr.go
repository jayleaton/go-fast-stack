package ui

import (
	"image/color"
	"strconv"
	"strings"

	"github.com/boombuler/barcode/qr"
)

// qrQuietZone is the light margin a scanner needs around the code, in modules.
const qrQuietZone = 4

// QRCode is a QR code ready for an inline SVG: one path of the dark modules,
// in module units, quiet zone included. Rendered server-side, so the TOTP
// secret never leaves the page for a QR service.
type QRCode struct {
	// Size is the side in modules, quiet zone included (the viewBox).
	Size int
	// Path is the d attribute: a rectangle per horizontal run of dark
	// modules. Only digits and path commands, never the encoded content.
	Path string
}

// ViewBox is the SVG viewBox of the code.
func (code QRCode) ViewBox() string {
	name := strconv.Itoa(code.Size)
	return "0 0 " + name + " " + name
}

// grayMidpoint splits a QR module's gray level into dark (below) and light.
const grayMidpoint = 128

// totpQR encodes an otpauth:// URI (anything else, like a value a plain post
// sent back, is not encoded). ok is false when there is nothing to show; the
// page keeps the manual key. The URI holds the secret: never log it.
func totpQR(uri string) (qrCode QRCode, ok bool) {
	if otpLink(uri) == "" {
		return QRCode{}, false
	}
	code, err := qr.Encode(uri, qr.M, qr.Auto)
	if err != nil {
		return QRCode{}, false
	}
	rectangle := code.Bounds()
	dark := func(left, right int) bool {
		return color.GrayModel.Convert(code.At(left, right)).(color.Gray).Y < grayMidpoint
	}
	var builder strings.Builder
	for row := rectangle.Min.Y; row < rectangle.Max.Y; row++ {
		for value := rectangle.Min.X; value < rectangle.Max.X; {
			if !dark(value, row) {
				value++
				continue
			}
			run := 1
			for value+run < rectangle.Max.X && dark(value+run, row) {
				run++
			}
			word := strconv.Itoa(run)
			builder.WriteString("M" + strconv.Itoa(value-rectangle.Min.X+qrQuietZone) + " " + strconv.Itoa(row-rectangle.Min.Y+qrQuietZone) + "h" + word + "v1h-" + word + "z")
			value += run
		}
	}
	return QRCode{Size: rectangle.Dx() + 2*qrQuietZone, Path: builder.String()}, true
}
