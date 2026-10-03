package hub_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"testing"

	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/pdfw"
	"github.com/giovannirco/platen/internal/raster"
)

func TestPrintFreshScanAndAddedPage(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	sc, err := f.hub.StartScan(ctx, hub.ScanRequest{Resolution: 75, Paper: "4x6"})
	if err != nil {
		t.Fatal(err)
	}
	for pages := 1; pages <= 2; pages++ {
		res, err := f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{ScanID: sc.ID}, DryRun: true})
		if err != nil {
			t.Fatalf("copy %d freshly scanned pages: %v", pages, err)
		}
		if res.Pages != pages || res.State != "not-printed" {
			t.Fatalf("copy uses stale document: %+v", res)
		}
		if pages == 1 {
			sc, err = f.hub.ScanNextPage(ctx, sc.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(f.raster.Jobs()) != 0 {
		t.Fatal("scan copy dry run printed")
	}
}

func TestPDFPageOrderOnPassthroughPrinter(t *testing.T) {
	// Distinct pages let us check the content received by the printer.
	var pages []pdfw.Page
	for _, shade := range []uint8{20, 220} {
		img := image.NewGray(image.Rect(0, 0, 300, 450))
		for i := range img.Pix {
			img.Pix[i] = shade
		}
		var jpegPage bytes.Buffer
		if err := jpeg.Encode(&jpegPage, img, nil); err != nil {
			t.Fatal(err)
		}
		pages = append(pages, pdfw.Page{JPEG: jpegPage.Bytes(), DPI: 75})
	}
	var doc bytes.Buffer
	if err := pdfw.Write(&doc, pages, pdfw.Info{Title: "Page order"}); err != nil {
		t.Fatal(err)
	}
	for _, ranges := range []bool{false, true} {
		t.Run(map[bool]string{false: "no page ranges", true: "page ranges"}[ranges], func(t *testing.T) {
			f := newFixture(t, nil)
			f.pdf.PageRanges = ranges
			res, err := f.hub.Print(context.Background(), hub.PrintRequest{
				Printer: "laser", Source: hub.Source{Data: doc.Bytes()}, Pages: "2,1", Quality: "draft", Color: "monochrome", Media: "4x6",
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Format != "image/pwg-raster" {
				t.Fatalf("reordered pages passed through unchanged: %+v", res)
			}
			printed, err := raster.Decode(bytes.NewReader(f.pdf.Jobs()[0].Document))
			if err != nil || len(printed) != 2 {
				t.Fatalf("printed pages: %d %v", len(printed), err)
			}
			for i, wantLight := range []bool{true, false} {
				p := printed[i]
				shade := p.Pix[(p.Height/2)*p.Width+p.Width/2]
				if (shade > 128) != wantLight {
					t.Errorf("printed page %d has the wrong content: gray=%d", i+1, shade)
				}
			}
		})
	}
	t.Run("PDF only", func(t *testing.T) {
		f := newFixture(t, nil)
		f.pdf.Formats = []string{"application/pdf"}
		_, err := f.hub.Print(context.Background(), hub.PrintRequest{Printer: "laser", Source: hub.Source{Data: doc.Bytes()}, Pages: "2,1"})
		if !errors.Is(err, hub.ErrUnsupported) || len(f.pdf.Jobs()) != 0 {
			t.Fatalf("a printer that cannot reorder must reject the job: %v", err)
		}
	})
}
