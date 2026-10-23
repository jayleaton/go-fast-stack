package imaging

import (
	"image"
	"math"
)

// resize scales src to width x height with the Catmull-Rom kernel, in bands
// of output rows, so its scratch memory stays under bandBudget whatever the
// source size. golang.org/x/image/draw's CatmullRom.Scale holds the whole
// horizontally scaled image instead (width x source height x 32 bytes: 198 MB
// for a 12 MP photo fitted to 2048 px).
//
// The result is the same as CatmullRom.Scale's, pixel for pixel: the same
// weights (newDistrib below mirrors x/image's), the same 16-bit premultiplied
// arithmetic and the same rounding. Bands cannot leave seams: each output row
// is computed from the source rows its own weights name, and a band scales
// every one of them, including the support rows it shares with its neighbours.
func resize(src image.Image, width, height int) *image.RGBA {
	bounds := src.Bounds()
	columns := newDistrib(width, bounds.Dx())
	rows := newDistrib(height, bounds.Dy())
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	maxBandRows := max(1, bandBudget/(width*bytesPerScratchPixel))
	scratch := make([][channels]float64, 0, min(maxBandRows, bounds.Dy())*width)
	readRow, opaque := rowReader(src)
	line := make([][channels]float64, bounds.Dx())

	for first := 0; first < height; {
		// Grow the band while the source rows it needs fit the budget (a
		// single output row always fits, whatever it needs).
		top, bottom := rows.span(first)
		last := first + 1
		for ; last < height; last++ {
			_, nextBottom := rows.span(last)
			if nextBottom-top > maxBandRows {
				break
			}
			bottom = nextBottom
		}
		// Horizontal pass: source rows [top, bottom) into scratch.
		if need := (bottom - top) * width; need > cap(scratch) {
			scratch = make([][channels]float64, need) // one output row reads more rows than the budget
		} else {
			scratch = scratch[:need]
		}
		for sourceRow := top; sourceRow < bottom; sourceRow++ {
			readRow(sourceRow, line)
			out := scratch[(sourceRow-top)*width:]
			for column := range width {
				var sum [channels]float64
				for _, contrib := range columns.weightsOf(column) {
					pixel := &line[contrib.coord]
					for channel := range channels {
						sum[channel] += pixel[channel] * contrib.weight
					}
				}
				for channel := range channels {
					out[column][channel] = sum[channel] * columns.sources[column].invTotalWeightFFFF
				}
				if opaque {
					out[column][alphaChannel] = 1 // exactly, as x/image does for opaque sources
				}
			}
		}
		// Vertical pass: output rows [first, last) from scratch.
		for row := first; row < last; row++ {
			invTotal := rows.sources[row].invTotalWeight
			offset := row * dst.Stride
			for column := range width {
				var sum [channels]float64
				for _, contrib := range rows.weightsOf(row) {
					pixel := &scratch[(contrib.coord-top)*width+column]
					for channel := range channels {
						sum[channel] += pixel[channel] * contrib.weight
					}
				}
				for channel := range alphaChannel { // premultiplied: no color above its alpha
					sum[channel] = min(sum[channel], sum[alphaChannel])
				}
				for channel := range channels {
					dst.Pix[offset+channel] = uint8(ftou(sum[channel]*invTotal) >> eightBits)
				}
				offset += channels
			}
		}
		first = last
	}
	return dst
}

const (
	channels             = 4       // R, G, B, A
	bytesPerScratchPixel = 4 * 8   // one float64 per channel
	bandBudget           = 8 << 20 // 8 MiB of scratch rows per band
	catmullRomSupport    = 2.0     // the kernel's half width in source pixels, before shrinking
	maxColor16           = 0xffff  // 16-bit color channel maximum, as color.Color.RGBA returns
	eightBits            = 8       // 16-bit to 8-bit channel
	halfPixel            = 0.5     // pixel centers sit at +0.5
	ycbcrChromaBias      = 128     // Cb and Cr are stored offset by 128
	lumaToRGB16          = 0x10101 // the factors of image/color's YCbCr.RGBA, in 16-bit fixed point
	crToRed              = 91881
	cbToGreen            = 22554
	crToGreen            = 46802
	cbToBlue             = 116130
	fixedPointShift      = 8
	channel8To16         = 0x101 // 0xff -> 0xffff
)

// Coefficients of the Catmull-Rom cubic (the BC-spline with B=0, C=0.5),
// inside one pixel of the center and from one to two pixels out.
const (
	innerCubic, innerSquare                         = 1.5, 2.5
	outerCubic, outerSquare, outerLinear, outerBase = 0.5, 2.5, 4, 2
)

// catmullRom is the Catmull-Rom kernel as x/image/draw defines it (same
// operations in the same order, so the same float64 results).
func catmullRom(distance float64) float64 {
	if distance < 1 {
		return float64((float64(innerCubic*distance)-innerSquare)*distance*distance) + 1
	}
	return float64((float64(float64(float64(-outerCubic*distance)+outerSquare)*distance)-outerLinear)*distance) + outerBase
}

// ftou is x/image/draw's: [0, 1] to [0, 0xffff], rounded.
func ftou(value float64) uint16 {
	scaled := int32(float64(maxColor16*value) + halfPixel)
	if scaled > maxColor16 {
		return maxColor16
	}
	if scaled > 0 {
		return uint16(scaled)
	}
	return 0
}

type contrib struct {
	coord  int
	weight float64
}

type source struct {
	first, last                        int // contribs[first:last]
	invTotalWeight, invTotalWeightFFFF float64
}

// distrib says which source columns (or rows), with which weights, make up
// each output column (or row).
type distrib struct {
	sources  []source
	contribs []contrib
}

// weightsOf is the source pixels and weights of output pixel index.
func (dist distrib) weightsOf(index int) []contrib {
	return dist.contribs[dist.sources[index].first:dist.sources[index].last]
}

// span is the source rows [top, bottom) output row index reads.
func (dist distrib) span(index int) (top, bottom int) {
	used := dist.weightsOf(index)
	if len(used) == 0 {
		return 0, 0
	}
	return used[0].coord, used[len(used)-1].coord + 1
}

// newDistrib mirrors x/image/draw's newDistrib for the Catmull-Rom kernel:
// srcLen source pixels over dstLen output pixels, the kernel widened when
// shrinking so every source pixel counts.
func newDistrib(dstLen, srcLen int) distrib {
	scale := float64(srcLen) / float64(dstLen)
	halfWidth, kernelArgScale := catmullRomSupport, 1.0
	if scale > 1 {
		halfWidth *= scale
		kernelArgScale = 1 / scale
	}
	sources := make([]source, dstLen)
	contribs := make([]contrib, 0, dstLen*int(math.Ceil(2*halfWidth+1)))
	for index := range sources {
		center := float64((float64(index)+halfPixel)*scale) - halfPixel
		low := max(int(math.Floor(center-halfWidth)), 0)
		high := min(int(math.Ceil(center+halfWidth)), srcLen)
		high = max(high, low)
		first := len(contribs)
		totalWeight := 0.0
		for coord := low; coord < high; coord++ {
			distance := math.Abs((center - float64(coord)) * kernelArgScale)
			if distance >= catmullRomSupport {
				continue
			}
			weight := catmullRom(distance)
			if weight == 0 {
				continue
			}
			totalWeight += weight
			contribs = append(contribs, contrib{coord, weight})
		}
		totalWeight = 1 / totalWeight
		sources[index] = source{first: first, last: len(contribs), invTotalWeight: totalWeight, invTotalWeightFFFF: totalWeight / maxColor16}
	}
	return distrib{sources, contribs}
}

// rowReader returns a function that fills line with source row (relative to
// the bounds) as 16-bit premultiplied RGBA, the values color.Color.RGBA
// returns, and whether every pixel is opaque by type. The common decoder
// outputs get a direct path.
func rowReader(src image.Image) (func(row int, line [][channels]float64), bool) {
	bounds := src.Bounds()
	switch img := src.(type) {
	case *image.YCbCr:
		return func(row int, line [][channels]float64) {
			sourceY := bounds.Min.Y + row
			for index := range line {
				sourceX := bounds.Min.X + index
				luma := int(img.Y[img.YOffset(sourceX, sourceY)]) * lumaToRGB16
				chromaOffset := img.COffset(sourceX, sourceY)
				cb := int(img.Cb[chromaOffset]) - ycbcrChromaBias
				cr := int(img.Cr[chromaOffset]) - ycbcrChromaBias
				line[index] = [channels]float64{
					clamp16((luma + crToRed*cr) >> fixedPointShift),
					clamp16((luma - cbToGreen*cb - crToGreen*cr) >> fixedPointShift),
					clamp16((luma + cbToBlue*cb) >> fixedPointShift),
					maxColor16,
				}
			}
		}, true
	case *image.RGBA:
		return func(row int, line [][channels]float64) {
			start := img.PixOffset(bounds.Min.X, bounds.Min.Y+row)
			for index := range line {
				pixel := img.Pix[start+index*channels : start+index*channels+channels]
				for channel := range channels {
					line[index][channel] = float64(uint32(pixel[channel]) * channel8To16)
				}
			}
		}, false
	case *image.NRGBA: // straight alpha, premultiplied as color.NRGBA.RGBA does
		return func(row int, line [][channels]float64) {
			start := img.PixOffset(bounds.Min.X, bounds.Min.Y+row)
			for index := range line {
				pixel := img.Pix[start+index*channels : start+index*channels+channels]
				alpha := uint32(pixel[alphaChannel]) * channel8To16
				for channel := range alphaChannel {
					line[index][channel] = float64(uint32(pixel[channel]) * channel8To16 * alpha / maxColor16)
				}
				line[index][alphaChannel] = float64(alpha)
			}
		}, false
	default:
		return func(row int, line [][channels]float64) {
			sourceY := bounds.Min.Y + row
			for index := range line {
				red, green, blue, alpha := src.At(bounds.Min.X+index, sourceY).RGBA()
				line[index] = [channels]float64{float64(red), float64(green), float64(blue), float64(alpha)}
			}
		}, false
	}
}

func clamp16(value int) float64 {
	return float64(min(max(value, 0), maxColor16))
}
