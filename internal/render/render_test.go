package render

import (
	"image"
	"image/color"
	"strings"
	"testing"
)

func TestLayoutText(t *testing.T) {
	a4 := Target{Width: 1240, Height: 1753, Margin: 70} // 150 dpi
	text := "Title\n\n" + strings.Repeat("word ", 60) + "\n\tindented\n" + strings.Repeat("line\n", 150)
	layout, err := LayoutText(text, a4, 150, TextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if layout.Pages() != 3 {
		t.Fatalf("%d pages, want 3", layout.Pages())
	}
	// The long paragraph is wrapped at a space, inside the page width.
	for _, line := range layout.pages[0] {
		if len(line) > 90 {
			t.Errorf("line not wrapped: %d characters", len(line))
		}
		if strings.HasPrefix(line, " ") && strings.TrimSpace(line) != "indented" {
			t.Errorf("wrapped line starts with a space: %q", line)
		}
	}
	img, err := layout.RenderPage(0)
	if err != nil {
		t.Fatal(err)
	}
	ink := 0
	rgba := img.(*image.RGBA)
	for i := 0; i < len(rgba.Pix); i += 4 {
		if rgba.Pix[i] < 128 {
			ink++
		}
	}
	if ink < 2000 {
		t.Errorf("the page looks empty (%d dark pixels)", ink)
	}
	if _, err := layout.RenderPage(3); err == nil {
		t.Error("page out of range should fail")
	}
	// A form feed starts a new page.
	ff, _ := LayoutText("one\n\f\ntwo", a4, 150, TextOptions{})
	if ff.Pages() != 2 {
		t.Errorf("form feed: %d pages", ff.Pages())
	}
	if _, err := LayoutText("x", Target{Width: 50, Height: 50}, 150, TextOptions{}); err == nil {
		t.Error("a sheet too small for text should fail")
	}
}

func TestFitImage(t *testing.T) {
	photo := image.NewRGBA(image.Rect(0, 0, 300, 200)) // landscape
	for i := 0; i < len(photo.Pix); i += 4 {
		photo.Pix[i], photo.Pix[i+3] = 255, 255 // red
	}
	sheet := Target{Width: 400, Height: 600, Margin: 20} // portrait
	red := func(img image.Image, x, y int) bool {
		r, g, _, _ := img.At(x, y).RGBA()
		return r>>8 > 200 && g>>8 < 60
	}
	fit := FitImage(photo, sheet)
	if b := fit.Bounds(); b.Dx() != 400 || b.Dy() != 600 {
		t.Fatalf("sheet size %v", b)
	}
	// Turned to portrait and scaled to 360 x 540 inside the margins.
	if !red(fit, 200, 300) || red(fit, 5, 300) || red(fit, 200, 5) || !red(fit, 30, 40) {
		t.Error("the picture is not fitted inside the margins")
	}
	fill := FitImage(photo, Target{Width: 400, Height: 600, Fill: true})
	if !red(fill, 2, 2) || !red(fill, 397, 597) {
		t.Error("the picture does not cover the sheet")
	}
	gray := FitImage(photo, Target{Width: 400, Height: 600, Gray: true})
	if _, ok := gray.(*image.Gray); !ok {
		t.Errorf("gray target gave %T", gray)
	}
	if th := Thumbnail(photo, 60); th.Bounds().Dx() != 60 || th.Bounds().Dy() != 40 {
		t.Errorf("thumbnail %v", th.Bounds())
	}
	if th := Thumbnail(photo, 1000); th != image.Image(photo) {
		t.Error("a picture smaller than the limit should be returned as it is")
	}
	_ = color.White
}
