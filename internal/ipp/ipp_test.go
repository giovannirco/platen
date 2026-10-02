package ipp_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/giovannirco/platen/internal/ipp"
	"github.com/giovannirco/platen/internal/testutil"
)

func TestParseMedia(t *testing.T) {
	for name, want := range map[string]struct {
		w, h   int
		label  string
		px600w int
	}{
		"iso_a4_210x297mm":   {21000, 29700, "A4", 4960},
		"na_letter_8.5x11in": {21590, 27940, "Letter", 5100},
		"na_index-4x6_4x6in": {10160, 15240, "4x6in", 2400},
		"jis_b5_182x257mm":   {18200, 25700, "JIS B5", 4299},
		"iso_a5_148x210mm":   {14800, 21000, "A5", 3496},
	} {
		m, ok := ipp.ParseMedia(name)
		if !ok || m.Width != want.w || m.Height != want.h || m.Label() != want.label {
			t.Errorf("%s: %+v label %q", name, m, m.Label())
		}
		if w, _ := m.Pixels(600); w != want.px600w {
			t.Errorf("%s at 600 dpi: %d px wide, want %d", name, w, want.px600w)
		}
	}
	// A4 in points, truncated the way CUPS does it.
	a4, _ := ipp.ParseMedia("iso_a4_210x297mm")
	if w, h := a4.Points(); w != 595 || h != 841 {
		t.Errorf("A4 points: %dx%d", w, h)
	}
	for _, bad := range []string{"", "a4", "iso_a4_210x297", "custom_x_0x10mm", "custom_x_axbmm"} {
		if _, ok := ipp.ParseMedia(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestClientAgainstPrinter(t *testing.T) {
	fake := testutil.NewRasterPrinter(t)
	ctx := context.Background()
	c, err := ipp.New(fake.URI(), ipp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.PrinterAttributes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != "idle" || !p.AcceptingJobs || p.MakeAndModel != "Platen Test Inkjet" || p.Supports("application/pdf") || !p.Supports("IMAGE/PWG-RASTER") {
		t.Errorf("printer: %+v", p)
	}
	if len(p.RasterDPI) != 1 || p.RasterDPI[0] != 600 || p.RasterBack != "rotated" || p.MaxCopies != 99 || len(p.Qualities) != 3 {
		t.Errorf("capabilities: dpi=%v back=%q copies=%d qualities=%v", p.RasterDPI, p.RasterBack, p.MaxCopies, p.Qualities)
	}
	if len(p.Markers) != 4 || p.Markers[1].Color != "#00CFFF" || p.Markers[2].Level != 8 {
		t.Errorf("markers: %+v", p.Markers)
	}

	doc := bytes.Repeat([]byte("raster data "), 5000)
	job, err := c.PrintJob(ctx, p, ipp.JobRequest{Name: "Report", Format: "image/pwg-raster", Copies: 3, Sides: "two-sided-long-edge", Quality: "high", Media: "iso_a4_210x297mm", ResolutionDPI: 600,
		// Not supported by this printer, so it must not be sent.
		PageRanges: [][2]int{{1, 2}}}, bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	got := fake.Jobs()[0]
	if job.ID != 1 || job.State != "processing" || !bytes.Equal(got.Document, doc) || got.Format != "image/pwg-raster" {
		t.Fatalf("job: %+v", job)
	}
	if got.Attrs["copies"] != "3" || got.Attrs["print-quality"] != "5" || got.Attrs["printer-resolution"] != "600x600dpi" || got.Attrs["page-ranges"] != "" {
		t.Errorf("job attributes: %v", got.Attrs)
	}
	// A streamed document (no known length) arrives intact too.
	if _, err := c.PrintJob(ctx, p, ipp.JobRequest{Format: "image/pwg-raster"}, struct{ *bytes.Reader }{bytes.NewReader(doc)}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fake.Jobs()[1].Document, doc) {
		t.Error("streamed document corrupted")
	}

	j, err := c.Job(ctx, 1)
	if err != nil || j.State != "completed" || !j.Done() {
		t.Errorf("job state: %+v %v", j, err)
	}
	jobs, err := c.Jobs(ctx, "completed", 10)
	if err != nil || len(jobs) != 2 || jobs[0].Name != "Report" {
		t.Errorf("jobs: %+v %v", jobs, err)
	}
	if err := c.CancelJob(ctx, 2); err != nil || !fake.Jobs()[1].Canceled {
		t.Errorf("cancel: %v", err)
	}
	var ippErr *ipp.Error
	if err := c.CancelJob(ctx, 99); !errors.As(err, &ippErr) {
		t.Errorf("cancel of a missing job: %v", err)
	}
	fake.NotAccepting = true
	if _, err := c.PrintJob(ctx, p, ipp.JobRequest{}, bytes.NewReader(doc)); !errors.As(err, &ippErr) {
		t.Errorf("print on a printer that refuses jobs: %v", err)
	}
}

func TestURIForms(t *testing.T) {
	for uri, want := range map[string]string{
		"ipp://printer.lan/ipp/print":        "ipp://printer.lan/ipp/print",
		"ipps://printer.lan:8443/ipp/print":  "ipps://printer.lan:8443/ipp/print",
		"http://cups.lan:631/printers/laser": "ipp://cups.lan:631/printers/laser",
		"https://cups.lan/printers/laser":    "ipps://cups.lan/printers/laser",
	} {
		c, err := ipp.New(uri, ipp.Options{})
		if err != nil || c.URI() != want {
			t.Errorf("%s -> %q, %v", uri, c.URI(), err)
		}
	}
	for _, bad := range []string{"usb://printer", "printer.lan", "ipp:///ipp/print"} {
		if _, err := ipp.New(bad, ipp.Options{}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
