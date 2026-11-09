// Package icon holds Habakkuk Desktop's icons: the app icon (the cream mark
// on an ink tile, from assets/brand/app-icon-1024.png) at any size, and the
// macOS menu bar template icons.
package icon

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"
	"sync"

	"golang.org/x/image/draw"
)

var (
	//go:embed app.png
	appPNG []byte
	// The menu bar icons: the mark in black on transparent, a template image
	// macOS tints for light and dark menu bars. Each TIFF holds the 22pt
	// image at @1x (22px) and @2x (44px), drawn on their own pixel grids.
	//go:embed tray.tiff
	trayTIFF []byte
	// The same with a dot in the corner: approvals are waiting, or there is
	// a problem.
	//go:embed tray-dot.tiff
	trayDotTIFF []byte
)

var source = sync.OnceValue(func() image.Image {
	img, err := png.Decode(bytes.NewReader(appPNG))
	if err != nil {
		panic(err) // embedded at build time; the tests decode it
	}
	return img
})

// Image returns the app icon as a size x size image: the ink tile fills it,
// with transparent rounded corners. This is the icon for Windows and Linux.
func Image(size int) image.Image {
	return scaled(size, size)
}

// MacImage returns the app icon laid out on Apple's macOS icon grid: the
// tile is 824/1024 of the canvas, centred, so it sits with the other icons
// in the Dock and Finder.
func MacImage(size int) image.Image {
	return scaled(size, (size*824+512)/1024)
}

// App is Image(size) as PNG.
func App(size int) []byte { return encode(Image(size)) }

// MacApp is MacImage(size) as PNG.
func MacApp(size int) []byte { return encode(MacImage(size)) }

// Tray returns the menu bar template icon (TIFF, 22pt @1x and @2x), with
// the corner dot when dot is true.
func Tray(dot bool) []byte {
	if dot {
		return trayDotTIFF
	}
	return trayTIFF
}

// scaled draws the tile at tile x tile pixels in the middle of a size x
// size transparent canvas.
func scaled(size, tile int) image.Image {
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	off := (size - tile) / 2
	draw.CatmullRom.Scale(dst, image.Rect(off, off, off+tile, off+tile), source(), source().Bounds(), draw.Src, nil)
	return dst
}

func encode(img image.Image) []byte {
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&b, img); err != nil {
		panic(err) // an in-memory image always encodes
	}
	return b.Bytes()
}
