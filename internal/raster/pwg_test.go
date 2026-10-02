package raster

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"testing"
)

// testImage has flat areas, a gradient and noise, so every branch of the row
// encoder (repeat runs, literal runs, repeated lines) is exercised.
func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 255, 255, 255}
			switch {
			case y < h/4: // blank paper
			case y < h/2: // gradient
				c = color.RGBA{uint8(x * 255 / w), uint8(y), 40, 255}
			case y < 3*h/4: // noise
				c = color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255}
			default: // stripes of equal pixels
				if (x/7)%2 == 0 {
					c = color.RGBA{0, 0, 0, 255}
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestRoundTripColor(t *testing.T) {
	src := testImage(301, 97)
	var buf bytes.Buffer
	w := NewWriter(&buf)
	page := Page{Image: src, Scale: 1, DPI: 300, MediaName: "iso_a4_210x297mm", PageSizePoints: [2]int{595, 841}, TotalPages: 1, Quality: 4}
	if err := w.WritePage(page); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	pages, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(pages))
	}
	p := pages[0]
	if p.Width != 301 || p.Height != 97 || p.DPI != 300 || p.Gray || p.MediaName != "iso_a4_210x297mm" || p.TotalPages != 1 {
		t.Fatalf("header mismatch: %+v", p)
	}
	for y := 0; y < 97; y++ {
		for x := 0; x < 301; x++ {
			want := src.RGBAAt(x, y)
			o := (y*301 + x) * 3
			if p.Pix[o] != want.R || p.Pix[o+1] != want.G || p.Pix[o+2] != want.B {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, p.Pix[o:o+3], want)
			}
		}
	}
}

func TestScaleAndGray(t *testing.T) {
	src := testImage(64, 40)
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WritePage(Page{Image: src, Scale: 2, DPI: 600, Gray: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.WritePage(Page{Image: src, Scale: 2, DPI: 600, Gray: true}); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	pages, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("got %d pages, want 2", len(pages))
	}
	p := pages[0]
	if p.Width != 128 || p.Height != 80 || !p.Gray || p.DPI != 600 {
		t.Fatalf("header mismatch: %dx%d gray=%t dpi=%d", p.Width, p.Height, p.Gray, p.DPI)
	}
	for y := 0; y < 80; y++ {
		for x := 0; x < 128; x++ {
			c := src.RGBAAt(x/2, y/2)
			want := uint8((299*int(c.R) + 587*int(c.G) + 114*int(c.B) + 500) / 1000)
			if got := p.Pix[y*128+x]; got != want {
				t.Fatalf("pixel (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

func TestDuplexBackSide(t *testing.T) {
	src := testImage(20, 10)
	cases := []struct {
		back         SheetBack
		tumble       bool
		flipX, flipY bool
	}{
		{BackNormal, false, false, false},
		{BackRotated, false, true, true},
		{BackRotated, true, false, false},
		{BackFlipped, false, false, true},
		{BackFlipped, true, true, false},
		{BackManualTumble, true, true, true},
		{BackManualTumble, false, false, false},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		if err := w.WritePage(Page{Image: src, DPI: 300, Duplex: true, Tumble: c.tumble, Back: true, SheetBack: c.back}); err != nil {
			t.Fatal(err)
		}
		w.Flush()
		pages, err := Decode(&buf)
		if err != nil {
			t.Fatal(err)
		}
		p := pages[0]
		wantCross, wantFeed := 1, 1
		if c.flipX {
			wantCross = -1
		}
		if c.flipY {
			wantFeed = -1
		}
		if p.CrossFeed != wantCross || p.Feed != wantFeed || !p.Duplex || p.Tumble != c.tumble {
			t.Errorf("%s tumble=%t: transforms %d/%d, want %d/%d", c.back, c.tumble, p.CrossFeed, p.Feed, wantCross, wantFeed)
		}
		sx, sy := 3, 2
		if c.flipX {
			sx = 19 - sx
		}
		if c.flipY {
			sy = 9 - sy
		}
		want := src.RGBAAt(sx, sy)
		o := (2*20 + 3) * 3
		if p.Pix[o] != want.R || p.Pix[o+1] != want.G || p.Pix[o+2] != want.B {
			t.Errorf("%s tumble=%t: pixel not transformed as expected", c.back, c.tumble)
		}
	}
}

func TestBlankPageIsSmall(t *testing.T) {
	// An empty A4 page at 600 dpi is 104 MB of pixels; it must compress to almost nothing.
	blank := image.NewGray(image.Rect(0, 0, 2480, 3507))
	for i := range blank.Pix {
		blank.Pix[i] = 0xFF
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WritePage(Page{Image: blank, Scale: 2, DPI: 600}); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if buf.Len() > 64<<10 {
		t.Fatalf("blank page encoded to %d bytes", buf.Len())
	}
	pages, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if pages[0].Width != 4960 || pages[0].Height != 7014 {
		t.Fatalf("got %dx%d", pages[0].Width, pages[0].Height)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode(bytes.NewReader([]byte("%PDF-1.4"))); err == nil {
		t.Fatal("expected an error for a non-raster stream")
	}
}
