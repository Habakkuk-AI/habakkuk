package icon

import (
	"bytes"
	"image/png"
	"testing"
)

func TestAppIcons(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		size int
	}{
		{"app", App(64), 64},
		{"app 256", App(256), 256},
		{"mac", MacApp(128), 128},
	} {
		img, err := png.Decode(bytes.NewReader(tc.data))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if b := img.Bounds(); b.Dx() != tc.size || b.Dy() != tc.size {
			t.Fatalf("%s: %v", tc.name, b)
		}
	}
	// The tile's corners are transparent (rounded), its middle is the ink.
	img := Image(64)
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Error("corner is opaque")
	}
	if r, g, b, a := img.At(32, 50).RGBA(); a != 0xffff || r>>8 > 0x20 || g>>8 > 0x20 || b>>8 > 0x20 {
		t.Errorf("below the mark is not ink: %x %x %x %x", r>>8, g>>8, b>>8, a>>8)
	}
	// On the macOS grid the tile leaves a margin: a point just inside the
	// edge of the full tile is transparent there.
	if _, _, _, a := MacImage(1024).At(60, 512).RGBA(); a != 0 {
		t.Error("the macOS icon has no margin")
	}
	if _, _, _, a := Image(1024).At(60, 512).RGBA(); a == 0 {
		t.Error("the full tile has a margin")
	}
}

func TestTrayIcons(t *testing.T) {
	for _, dot := range []bool{false, true} {
		b := Tray(dot)
		// Big-endian or little-endian TIFF.
		if len(b) < 8 || !(bytes.HasPrefix(b, []byte("MM\x00*")) || bytes.HasPrefix(b, []byte("II*\x00"))) {
			t.Fatalf("dot=%v: not a TIFF", dot)
		}
	}
	if bytes.Equal(Tray(false), Tray(true)) {
		t.Error("the dot changes nothing")
	}
}
