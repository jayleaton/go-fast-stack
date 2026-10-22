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
func (q QRCode) ViewBox() string {
	n := strconv.Itoa(q.Size)
	return "0 0 " + n + " " + n
}

// totpQR encodes an otpauth:// URI (anything else, like a value a plain post
// sent back, is not encoded). ok is false when there is nothing to show; the
// page keeps the manual key. The URI holds the secret: never log it.
func totpQR(uri string) (q QRCode, ok bool) {
	if otpLink(uri) == "" {
		return QRCode{}, false
	}
	code, err := qr.Encode(uri, qr.M, qr.Auto)
	if err != nil {
		return QRCode{}, false
	}
	b := code.Bounds()
	dark := func(x, y int) bool {
		return color.GrayModel.Convert(code.At(x, y)).(color.Gray).Y < 128
	}
	var p strings.Builder
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; {
			if !dark(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < b.Max.X && dark(x+run, y) {
				run++
			}
			w := strconv.Itoa(run)
			p.WriteString("M" + strconv.Itoa(x-b.Min.X+qrQuietZone) + " " + strconv.Itoa(y-b.Min.Y+qrQuietZone) + "h" + w + "v1h-" + w + "z")
			x += run
		}
	}
	return QRCode{Size: b.Dx() + 2*qrQuietZone, Path: p.String()}, true
}
