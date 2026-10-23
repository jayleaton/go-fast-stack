package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
)

// The parsers below read hostile input: every offset and length comes from
// the file, so each one is bounds-checked before use and a malformed file
// just means "no orientation".

// EXIF orientations (TIFF tag 0x0112): how the stored pixels must be turned
// to show the picture upright.
const (
	orientUpright    = 1 // as stored
	orientMirror     = 2 // mirror left-right
	orientRotate180  = 3
	orientFlip       = 4 // mirror top-bottom
	orientTranspose  = 5 // mirror along the top-left/bottom-right diagonal
	orientRotate90   = 6 // rotate 90° clockwise
	orientTransverse = 7 // mirror along the other diagonal
	orientRotate270  = 8 // rotate 90° counter-clockwise
)

// File layout numbers the parsers rely on.
const (
	jpegMarkerPrefix = 0xff // every JPEG marker starts with it
	jpegSOILen       = 2    // the start-of-image marker before the first segment
	jpegMarkerLen    = 2    // 0xff + marker byte; a segment length field is 2 bytes too
	jpegSegmentHead  = 4    // marker (2) + length (2)
	jpegMarkerTEM    = 0x01 // markers without a length field: TEM, RST0-7
	jpegMarkerRST0   = 0xd0
	jpegMarkerRST7   = 0xd7
	jpegMarkerSOS    = 0xda // start of scan: no metadata after it
	jpegMarkerEOI    = 0xd9
	jpegMarkerAPP1   = 0xe1 // where Exif lives
	exifPrefixLen    = 6    // "Exif\x00\x00"

	pngSignatureLen = 8
	pngChunkHead    = 8 // length (4) + type (4)
	pngChunkCRC     = 4

	webpChunksStart = 12 // after "RIFF" size "WEBP"
	webpChunkHead   = 8  // fourcc (4) + size (4)

	tiffHeaderLen   = 8  // byte order (2) + 42 (2) + IFD0 offset (4)
	tiffMagic       = 42 // the TIFF version number every header carries
	tiffEntryLen    = 12 // tag (2) + type (2) + count (4) + value (4)
	tiffCountLen    = 2  // the IFD entry count
	tiffTagOrient   = 0x0112
	tiffTypeShort   = 3
	tiffValueOffset = 8 // where a SHORT value sits inside an entry

	bytesPerPixel = 4 // RGBA
)

// jpegExif returns the TIFF payload of a JPEG's first APP1 Exif segment, or nil.
func jpegExif(data []byte) []byte {
	offset := jpegSOILen
	for offset+jpegSegmentHead <= len(data) && data[offset] == jpegMarkerPrefix {
		marker := data[offset+1]
		switch {
		case marker == jpegMarkerPrefix: // fill byte
			offset++
			continue
		case marker == jpegMarkerSOS || marker == jpegMarkerEOI:
			return nil
		case marker == jpegMarkerTEM || marker >= jpegMarkerRST0 && marker <= jpegMarkerRST7: // no length
			offset += jpegMarkerLen
			continue
		}
		length := int(binary.BigEndian.Uint16(data[offset+jpegMarkerLen:]))
		if length < jpegMarkerLen || length > len(data)-offset-jpegMarkerLen {
			return nil
		}
		segment := data[offset+jpegSegmentHead : offset+jpegMarkerLen+length]
		if marker == jpegMarkerAPP1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return segment[exifPrefixLen:]
		}
		offset += jpegMarkerLen + length
	}
	return nil
}

// pngExif returns the payload of a PNG's eXIf chunk, or nil.
func pngExif(data []byte) []byte {
	for offset := pngSignatureLen; offset+pngChunkHead <= len(data); {
		length := int(binary.BigEndian.Uint32(data[offset:]))
		if length < 0 || length > len(data)-offset-pngChunkHead-pngChunkCRC {
			return nil
		}
		if string(data[offset+4:offset+pngChunkHead]) == "eXIf" {
			return data[offset+pngChunkHead : offset+pngChunkHead+length]
		}
		offset += pngChunkHead + length + pngChunkCRC
	}
	return nil
}

// webpExif returns the TIFF payload of a WebP's EXIF chunk, or nil.
func webpExif(data []byte) []byte {
	for offset := webpChunksStart; offset+webpChunkHead <= len(data); {
		length := int(binary.LittleEndian.Uint32(data[offset+4:]))
		if length < 0 || length > len(data)-offset-webpChunkHead {
			return nil
		}
		if string(data[offset:offset+4]) == "EXIF" {
			return bytes.TrimPrefix(data[offset+webpChunkHead:offset+webpChunkHead+length], []byte("Exif\x00\x00")) // some encoders keep the JPEG prefix
		}
		offset += webpChunkHead + length + length&1 // chunks pad to an even size
	}
	return nil
}

// tiffOrientation reads the orientation tag from IFD0 of a TIFF/EXIF
// payload; orientUpright when absent or malformed.
func tiffOrientation(tiff []byte) int {
	if len(tiff) < tiffHeaderLen {
		return orientUpright
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return orientUpright
	}
	if order.Uint16(tiff[2:]) != tiffMagic {
		return orientUpright
	}
	ifd := order.Uint32(tiff[4:])
	if ifd > uint32(len(tiff)-tiffCountLen) {
		return orientUpright
	}
	entries := tiff[ifd+tiffCountLen:]
	for range min(int(order.Uint16(tiff[ifd:])), len(entries)/tiffEntryLen) {
		entry := entries[:tiffEntryLen]
		entries = entries[tiffEntryLen:]
		if order.Uint16(entry) == tiffTagOrient && order.Uint16(entry[2:]) == tiffTypeShort {
			if value := int(order.Uint16(entry[tiffValueOffset:])); value >= orientUpright && value <= orientRotate270 {
				return value
			}
			return orientUpright
		}
	}
	return orientUpright
}

// orient returns src turned upright for EXIF orientation (see the orient*
// constants). src must sit at the origin with a tight stride (fit
// guarantees it).
func orient(src *image.RGBA, orientation int) *image.RGBA {
	if orientation <= orientUpright || orientation > orientRotate270 {
		return src
	}
	width, height := src.Rect.Dx(), src.Rect.Dy()
	dstWidth, dstHeight := width, height
	if orientation >= orientTranspose { // the four that swap the sides
		dstWidth, dstHeight = height, width
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	for row := range height {
		for col := range width {
			dstCol, dstRow := col, row
			switch orientation {
			case orientMirror:
				dstCol = width - 1 - col
			case orientRotate180:
				dstCol, dstRow = width-1-col, height-1-row
			case orientFlip:
				dstRow = height - 1 - row
			case orientTranspose:
				dstCol, dstRow = row, col
			case orientRotate90:
				dstCol, dstRow = height-1-row, col
			case orientTransverse:
				dstCol, dstRow = height-1-row, width-1-col
			case orientRotate270:
				dstCol, dstRow = row, width-1-col
			}
			copy(dst.Pix[(dstRow*dstWidth+dstCol)*bytesPerPixel:][:bytesPerPixel], src.Pix[(row*width+col)*bytesPerPixel:][:bytesPerPixel])
		}
	}
	return dst
}
