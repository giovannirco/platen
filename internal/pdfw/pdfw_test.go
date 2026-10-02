package pdfw_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"github.com/giovannirco/platen/internal/pdfw"
	"github.com/giovannirco/platen/internal/render"
)

func picture(t *testing.T, w, h int, gray bool) []byte {
	t.Helper()
	var img image.Image
	if gray {
		g := image.NewGray(image.Rect(0, 0, w, h))
		for i := range g.Pix {
			g.Pix[i] = 0xFF
		}
		for y := h / 4; y < h/2; y++ {
			for x := w / 4; x < w/2; x++ {
				g.SetGray(x, y, color.Gray{Y: 0})
			}
		}
		img = g
	} else {
		c := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := 0; i < len(c.Pix); i += 4 {
			c.Pix[i], c.Pix[i+1], c.Pix[i+2], c.Pix[i+3] = 200, 30, 30, 255
		}
		img = c
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The written file is opened with PDFium, the same engine browsers use, which
// checks that it is a well-formed PDF with the right pages.
func TestWriteIsReadableByPDFium(t *testing.T) {
	var out bytes.Buffer
	err := pdfw.Write(&out, []pdfw.Page{
		{JPEG: picture(t, 1240, 1754, true), DPI: 150}, // A4 portrait
		{JPEG: picture(t, 600, 400, false), DPI: 100},  // 6 x 4 inch landscape
	}, pdfw.Info{Title: "Conta de luz (março)", Author: "Gio (test)", Created: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("%PDF-1.4")) || !bytes.Contains(out.Bytes(), []byte("/Title <FEFF")) || !bytes.Contains(out.Bytes(), []byte(`/Author (Gio \(test\))`)) {
		t.Error("header or document information not written as expected")
	}
	engine := render.NewEngine()
	defer engine.Close()
	pdf, err := engine.OpenPDF(context.Background(), out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer pdf.Close()
	if pdf.Pages() != 2 {
		t.Fatalf("%d pages, want 2", pdf.Pages())
	}
	if w, h, _ := pdf.PageSize(0); w < 595 || w > 596 || h < 841 || h > 842 {
		t.Errorf("page 1 is %.1f x %.1f pt, want A4", w, h)
	}
	if w, h, _ := pdf.PageSize(1); w != 432 || h != 288 {
		t.Errorf("page 2 is %.1f x %.1f pt, want 432 x 288", w, h)
	}
	// Render page 1 small and look for the black square.
	img, err := pdf.RenderPage(0, render.Target{Width: 310, Height: 438, Gray: true})
	if err != nil {
		t.Fatal(err)
	}
	g := img.(*image.Gray)
	if g.GrayAt(115, 160).Y > 60 || g.GrayAt(250, 380).Y < 200 {
		t.Errorf("rendered page does not show the picture: %d / %d", g.GrayAt(115, 160).Y, g.GrayAt(250, 380).Y)
	}
	// A landscape page on a portrait sheet is turned to fit.
	img, err = pdf.RenderPage(1, render.Target{Width: 310, Height: 438})
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 310 || b.Dy() != 438 {
		t.Errorf("sheet size %v", b)
	}
	if r, _, _, _ := img.At(155, 219).RGBA(); r>>8 < 150 {
		t.Error("the turned page is not on the sheet")
	}
}

func TestWriteRejectsNonJPEG(t *testing.T) {
	if err := pdfw.Write(&bytes.Buffer{}, []pdfw.Page{{JPEG: []byte("not a picture")}}, pdfw.Info{}); err == nil {
		t.Error("expected an error")
	}
	if err := pdfw.Write(&bytes.Buffer{}, nil, pdfw.Info{}); err == nil {
		t.Error("expected an error for an empty document")
	}
}
