package computer

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
)

// MaxShot is the largest width or height of a screenshot the model sees;
// bigger screens are scaled down and coordinates mapped back.
const MaxShot = 1366

// maxImageBytes keeps an encoded screenshot under the providers' 5 MB
// image limit once base64-encoded.
const maxImageBytes = 3_500_000

// geometry relates the three coordinate spaces: the image the model last
// saw (shot), the pixels captured (pix) and the space input commands take
// (in), which is points rather than pixels on a Retina Mac.
type geometry struct {
	shotW, shotH int
	pixW, pixH   int
	inW, inH     int
}

// fit is the size w×h scaled down, keeping its aspect, so neither side
// exceeds limit.
func fit(w, h, limit int) (int, int) {
	s := math.Min(1, math.Min(float64(limit)/float64(w), float64(limit)/float64(h)))
	return max(1, int(math.Round(float64(w)*s))), max(1, int(math.Round(float64(h)*s)))
}

// scale is the factor from screen pixels to the shot.
func (g geometry) scale() float64 { return float64(g.shotW) / float64(g.pixW) }

// toInput maps a point in the shot to input coordinates.
func (g geometry) toInput(x, y int) (int, int, error) {
	if g.shotW == 0 {
		return 0, 0, fmt.Errorf("the screen size is not known yet; take a screenshot first")
	}
	if x < 0 || y < 0 || x >= g.shotW || y >= g.shotH {
		return 0, 0, fmt.Errorf("(%d, %d) is outside the screenshot, which is %dx%d", x, y, g.shotW, g.shotH)
	}
	ix := int(math.Round(float64(x) * float64(g.inW) / float64(g.shotW)))
	iy := int(math.Round(float64(y) * float64(g.inH) / float64(g.shotH)))
	return min(ix, g.inW-1), min(iy, g.inH-1), nil
}

// fromInput maps input coordinates (the pointer's position) to the shot.
func (g geometry) fromInput(x, y int) (int, int) {
	if g.inW == 0 || g.inH == 0 {
		return x, y
	}
	return int(math.Round(float64(x) * float64(g.shotW) / float64(g.inW))), int(math.Round(float64(y) * float64(g.shotH) / float64(g.inH)))
}

// toPixels maps a rectangle in the shot to captured pixels.
func (g geometry) toPixels(r image.Rectangle) image.Rectangle {
	sx := float64(g.pixW) / float64(g.shotW)
	sy := float64(g.pixH) / float64(g.shotH)
	return image.Rect(int(float64(r.Min.X)*sx), int(float64(r.Min.Y)*sy),
		int(math.Ceil(float64(r.Max.X)*sx)), int(math.Ceil(float64(r.Max.Y)*sy))).Intersect(image.Rect(0, 0, g.pixW, g.pixH))
}

// decodeShot reads a captured screenshot into RGBA.
func decodeShot(data []byte) (*image.RGBA, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unreadable screenshot: %w", err)
	}
	if rgba, ok := img.(*image.RGBA); ok && rgba.Rect.Min == (image.Point{}) {
		return rgba, nil
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Rect, img, b.Min, draw.Src)
	return out, nil
}

// resize scales src to w×h by averaging the source pixels each target pixel
// covers (a box filter), which keeps text legible when shrinking.
func resize(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	if sw == w && sh == h {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	spans := func(n, sn int) ([]int, []int) {
		lo, hi := make([]int, n), make([]int, n)
		for i := range n {
			lo[i] = i * sn / n
			hi[i] = max((i+1)*sn/n, lo[i]+1)
		}
		return lo, hi
	}
	x0, x1 := spans(w, sw)
	y0, y1 := spans(h, sh)
	for y := range h {
		for x := range w {
			var r, g, b, a, n uint32
			for sy := y0[y]; sy < y1[y]; sy++ {
				i := src.PixOffset(src.Rect.Min.X+x0[x], src.Rect.Min.Y+sy)
				for sx := x0[x]; sx < x1[x]; sx++ {
					p := src.Pix[i : i+4 : i+4]
					r, g, b, a = r+uint32(p[0]), g+uint32(p[1]), b+uint32(p[2]), a+uint32(p[3])
					n++
					i += 4
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(b/n), uint8(a/n)
		}
	}
	return dst
}

// encodeShot encodes a screenshot for the model: PNG, or JPEG when the PNG
// would be too large.
func encodeShot(img image.Image) ([]byte, string, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	if buf.Len() <= maxImageBytes {
		return buf.Bytes(), "image/png", nil
	}
	for _, q := range []int{85, 70, 50} {
		buf.Reset()
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, "", err
		}
		if buf.Len() <= maxImageBytes {
			break
		}
	}
	return buf.Bytes(), "image/jpeg", nil
}
