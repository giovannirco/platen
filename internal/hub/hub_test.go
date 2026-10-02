package hub_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/pdfw"
	"github.com/giovannirco/platen/internal/raster"
	"github.com/giovannirco/platen/internal/testutil"
)

type fixture struct {
	hub       *hub.Hub
	raster    *testutil.Printer
	pdf       *testutil.Printer
	scanner   *testutil.Scanner
	paperless *testutil.Paperless
}

func newFixture(t *testing.T, tweak func(*config.Config)) *fixture {
	t.Helper()
	f := &fixture{
		raster: testutil.NewRasterPrinter(t), pdf: testutil.NewPDFPrinter(t),
		scanner: testutil.NewScanner(t), paperless: testutil.NewPaperless(t),
	}
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Printers = []config.Printer{
		{ID: "inkjet", Name: "Inkjet", URI: f.raster.URI(), Default: true},
		{ID: "laser", Name: "Laser", URI: f.pdf.URI()},
	}
	cfg.Scanners = []config.Scanner{{ID: "flatbed", Name: "Flatbed", URL: f.scanner.BaseURL()}}
	cfg.Paperless = config.Paperless{URL: f.paperless.URL, Token: testutil.PaperlessToken}
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	f.hub = h
	return f
}

// testPDF builds a PDF with the given number of A4 pages.
func testPDF(t *testing.T, pages int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 620, 877)) // A4 at 75 dpi
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	for y := 100; y < 140; y++ {
		for x := 80; x < 540; x++ {
			img.SetGray(x, y, color.Gray{Y: 20})
		}
	}
	var page bytes.Buffer
	if err := jpeg.Encode(&page, img, nil); err != nil {
		t.Fatal(err)
	}
	list := make([]pdfw.Page, pages)
	for i := range list {
		list[i] = pdfw.Page{JPEG: page.Bytes(), DPI: 75}
	}
	var out bytes.Buffer
	if err := pdfw.Write(&out, list, pdfw.Info{Title: "test"}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDevices(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	printers := f.hub.Printers(ctx)
	if len(printers) != 2 || !printers[0].Online || printers[0].State != "idle" {
		t.Fatalf("printers: %+v", printers)
	}
	p := printers[0]
	if len(p.Markers) != 4 || p.Markers[2].Level != 8 || !strings.Contains(p.Summary, "Magenta 8%") {
		t.Errorf("ink levels not reported: %+v / %q", p.Markers, p.Summary)
	}
	if p.Supports("application/pdf") || !printers[1].Supports("application/pdf") {
		t.Error("format support mixed up between the two printers")
	}
	scanners := f.hub.Scanners(ctx)
	if len(scanners) != 1 || !scanners[0].Online || scanners[0].Sources[0] != "flatbed" || scanners[0].State != "Idle" {
		t.Fatalf("scanners: %+v", scanners)
	}
	if _, err := f.hub.Printer(ctx, "nope"); !errors.Is(err, hub.ErrNotFound) {
		t.Errorf("unknown printer: got %v", err)
	}
}

func TestPrintPDFOnRasterPrinter(t *testing.T) {
	f := newFixture(t, nil)
	res, err := f.hub.Print(context.Background(), hub.PrintRequest{
		Source: hub.Source{Data: testPDF(t, 3), Name: "report.pdf"}, Pages: "2-3", Duplex: "long-edge", Color: "monochrome", Quality: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "image/pwg-raster" || res.Pages != 2 || res.DocumentPages != 3 || res.Sheets != 1 || res.PrinterJobID != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	jobs := f.raster.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("printer received %d jobs", len(jobs))
	}
	job := jobs[0]
	if job.Format != "image/pwg-raster" || job.Name != "report.pdf" || job.Attrs["sides"] != "two-sided-long-edge" || job.Attrs["print-color-mode"] != "monochrome" {
		t.Errorf("job attributes: format=%q name=%q attrs=%v", job.Format, job.Name, job.Attrs)
	}
	pages, err := raster.Decode(bytes.NewReader(job.Document))
	if err != nil {
		t.Fatalf("the printer got an invalid raster stream: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("raster has %d pages, want 2", len(pages))
	}
	first, back := pages[0], pages[1]
	// Draft is rendered at 150 dpi and sent at the printer's 600 dpi.
	if first.DPI != 600 || first.Width != 4960 || !first.Gray || !first.Duplex || first.MediaName != "iso_a4_210x297mm" {
		t.Errorf("first page: %dx%d %d dpi gray=%t duplex=%t media=%q", first.Width, first.Height, first.DPI, first.Gray, first.Duplex, first.MediaName)
	}
	// The fake printer wants the back side rotated, like the real one this was written for.
	if back.CrossFeed != -1 || back.Feed != -1 {
		t.Errorf("back side not rotated: %d/%d", back.CrossFeed, back.Feed)
	}
	// The page has a dark bar near the top; on the rotated back side it is near the bottom.
	dark := func(p raster.DecodedPage, yFrac float64) bool {
		y := int(float64(p.Height) * yFrac)
		return p.Pix[y*p.Width+p.Width/2] < 100
	}
	if !dark(first, 0.137) || dark(first, 0.863) {
		t.Error("front page content is not where it should be")
	}
	if dark(back, 0.137) || !dark(back, 0.863) {
		t.Error("back page content is not rotated")
	}
	if h := f.hub.History(10); len(h) != 1 || h[0].Sheets != 1 || h[0].Source != "upload" {
		t.Errorf("history: %+v", h)
	}
}

func TestPrintPDFOnPDFPrinterIsUntouched(t *testing.T) {
	f := newFixture(t, nil)
	doc := testPDF(t, 2)
	res, err := f.hub.Print(context.Background(), hub.PrintRequest{Printer: "laser", Source: hub.Source{Data: doc}, Copies: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "application/pdf" || res.Sheets != 4 {
		t.Fatalf("unexpected result: %+v", res)
	}
	job := f.pdf.Jobs()[0]
	if !bytes.Equal(job.Document, doc) {
		t.Error("the PDF was changed on its way to a printer that takes PDF")
	}
	if job.Attrs["copies"] != "2" {
		t.Errorf("copies attribute: %v", job.Attrs)
	}
}

func TestPrintTextAndPictures(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	long := strings.Repeat("A line of text that is long enough to be wrapped at the right margin of the page.\n", 200)
	res, err := f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{Text: long}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages < 2 || res.Format != "image/pwg-raster" || res.State != "not-printed" {
		t.Errorf("text dry run: %+v", res)
	}
	if len(f.raster.Jobs()) != 0 {
		t.Fatal("a dry run reached the printer")
	}

	var jpg bytes.Buffer
	_ = jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 400, 300)), nil)
	res, err = f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{Data: jpg.Bytes(), Name: "photo.jpg"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != "image/jpeg" {
		t.Errorf("a JPEG should go to a JPEG-capable printer as it is, got %s", res.Format)
	}
	if !bytes.Equal(f.raster.Jobs()[0].Document, jpg.Bytes()) {
		t.Error("the JPEG was re-encoded")
	}
}

func TestPrintGuards(t *testing.T) {
	f := newFixture(t, func(c *config.Config) {
		c.Limits.ConfirmAboveSheets, c.Limits.MaxSheets, c.Limits.MaxCopies = 2, 8, 5
	})
	ctx := context.Background()
	doc := hub.Source{Data: testPDF(t, 3)}

	_, err := f.hub.Print(ctx, hub.PrintRequest{Source: doc})
	var need *hub.ConfirmationRequired
	if !errors.As(err, &need) || need.Sheets != 3 || need.Pages != 3 {
		t.Fatalf("expected a confirmation request for 3 sheets, got %v", err)
	}
	if len(f.raster.Jobs()) != 0 {
		t.Fatal("printed without confirmation")
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: doc, Confirm: true}); err != nil {
		t.Fatalf("confirmed job failed: %v", err)
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: doc, Duplex: "long-edge"}); err != nil {
		t.Errorf("3 pages on 2 sheets is at the threshold and should print: %v", err)
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: doc, Copies: 3, Confirm: true}); !errors.Is(err, hub.ErrLimitExceeded) {
		t.Errorf("9 sheets should exceed max_sheets, got %v", err)
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: doc, Copies: 6}); !errors.Is(err, hub.ErrLimitExceeded) {
		t.Errorf("6 copies should exceed max_copies, got %v", err)
	}
	for name, req := range map[string]hub.PrintRequest{
		"no source":    {},
		"two sources":  {Source: hub.Source{Text: "x", URL: "https://example.com/a.pdf"}},
		"bad pages":    {Source: doc, Pages: "7-9"},
		"bad duplex":   {Source: doc, Duplex: "sideways"},
		"bad media":    {Source: doc, Media: "papyrus"},
		"not on offer": {Source: doc, Media: "legal"},
	} {
		if _, err := f.hub.Print(ctx, req); !errors.Is(err, hub.ErrInvalid) {
			t.Errorf("%s: got %v, want an invalid-request error", name, err)
		}
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{Data: []byte("PK\x03\x04 a zip file")}}); !errors.Is(err, hub.ErrUnsupported) {
		t.Errorf("zip file: got %v", err)
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{Path: "/etc/hosts"}}); !errors.Is(err, hub.ErrNotAllowed) {
		t.Errorf("file outside allowed dirs: got %v", err)
	}
	// A URL that points into the private network is refused by default.
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{URL: f.paperless.URL + "/x.pdf"}}); !errors.Is(err, hub.ErrNotAllowed) {
		t.Errorf("loopback URL: got %v", err)
	}
}

func TestPrintFromAllowedDirAndPaperless(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, func(c *config.Config) { c.Fetch.AllowedDirs = []string{dir} })
	ctx := context.Background()
	doc := testPDF(t, 1)
	if err := os.WriteFile(dir+"/letter.pdf", doc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/.secret", doc, 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := f.hub.Print(ctx, hub.PrintRequest{Printer: "laser", Source: hub.Source{Path: dir + "/letter.pdf"}}); err != nil || res.Title != "letter.pdf" {
		t.Errorf("allowed file: %+v, %v", res, err)
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{Path: dir + "/.secret"}}); !errors.Is(err, hub.ErrNotAllowed) {
		t.Errorf("hidden file: got %v", err)
	}
	if _, err := f.hub.Print(ctx, hub.PrintRequest{Source: hub.Source{Path: dir + "/../../etc/hosts"}}); err == nil {
		t.Error("path traversal was accepted")
	}

	id := f.paperless.AddDocument("Contract", doc)
	res, err := f.hub.Print(ctx, hub.PrintRequest{Printer: "laser", Source: hub.Source{PaperlessID: id}})
	if err != nil {
		t.Fatal(err)
	}
	if jobs := f.pdf.Jobs(); !bytes.Equal(jobs[len(jobs)-1].Document, doc) || res.Pages != 1 {
		t.Error("the Paperless document did not reach the printer intact")
	}
}

func TestScanSessionAndPaperless(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	sc, err := f.hub.StartScan(ctx, hub.ScanRequest{Resolution: 150, Color: "gray", Title: "Bill"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Pages) != 1 || sc.Pages[0].Width != 1240 || sc.Pages[0].Height != 1754 || sc.Source != "flatbed" {
		t.Fatalf("first page: %+v", sc)
	}
	if req := f.scanner.Requests[0]; !strings.Contains(req, "<scan:ColorMode>Grayscale8</scan:ColorMode>") || strings.Contains(req, "Intent") {
		t.Errorf("unexpected scan settings sent:\n%s", req)
	}
	if sc, err = f.hub.ScanNextPage(ctx, sc.ID); err != nil || len(sc.Pages) != 2 {
		t.Fatalf("second page: %v", err)
	}
	first := sc.Pages[0].ID
	if sc, err = f.hub.MoveScanPage(sc.ID, first, 1); err != nil || sc.Pages[1].ID != first {
		t.Fatalf("move page: %v", err)
	}
	if _, err := f.hub.FinishScan(sc.ID, "jpeg"); !errors.Is(err, hub.ErrInvalid) {
		t.Errorf("two pages as one JPEG: got %v", err)
	}
	if sc, err = f.hub.FinishScan(sc.ID, "pdf"); err != nil || sc.Document.Pages != 2 {
		t.Fatalf("finish: %v", err)
	}
	pdf, err := os.ReadFile(sc.Document.File)
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) {
		t.Fatalf("document: %v", err)
	}
	if name := hub.ScanFilename(sc); name != "Bill.pdf" {
		t.Errorf("file name %q", name)
	}
	thumb, err := f.hub.PageImage(sc.ID, first, 480)
	if cfg, err2 := jpeg.DecodeConfig(bytes.NewReader(thumb)); err != nil || err2 != nil || cfg.Height != 480 {
		t.Errorf("preview: %v %v %+v", err, err2, cfg)
	}

	// A scan can be printed: together that is a photocopy.
	if res, err := f.hub.Print(ctx, hub.PrintRequest{Printer: "laser", Source: hub.Source{ScanID: sc.ID}}); err != nil || res.Pages != 2 {
		t.Errorf("print scan: %+v %v", res, err)
	}

	sc, err = f.hub.FileScan(ctx, sc.ID, hub.FileRequest{
		Title: "Electricity bill", Tags: []string{"Bills", "house"}, Correspondent: "energy company", DocumentType: "Invoice", Created: "2026-09-15", Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Paperless == nil || sc.Paperless.Status != "success" || sc.Paperless.DocumentID == 0 || !strings.Contains(sc.Paperless.URL, "/documents/") {
		t.Fatalf("filing state: %+v", sc.Paperless)
	}
	docs := f.paperless.Docs()
	if len(docs) != 1 {
		t.Fatalf("paperless has %d documents", len(docs))
	}
	d := docs[0]
	if d.Title != "Electricity bill" || d.Created != "2026-09-15" || d.Filename != "Bill.pdf" || !bytes.Equal(d.Data, pdf) {
		t.Errorf("uploaded document: title=%q created=%q file=%q", d.Title, d.Created, d.Filename)
	}
	// "Bills" matched the existing tag "bills"; "house" and the document type were created;
	// the correspondent matched regardless of case.
	if len(d.Tags) != 2 || d.Tags[0] != 1 || d.Correspondent != 1 || d.DocumentType == 0 {
		t.Errorf("metadata ids: tags=%v correspondent=%d type=%d", d.Tags, d.Correspondent, d.DocumentType)
	}
	if tags := f.paperless.Names("tags"); len(tags) != 2 || tags[1] != "house" {
		t.Errorf("tags after filing: %v", tags)
	}

	if err := f.hub.DeleteScan(sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Scan(sc.ID); !errors.Is(err, hub.ErrNotFound) {
		t.Errorf("deleted scan still there: %v", err)
	}
}

func TestScanErrors(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	for name, req := range map[string]hub.ScanRequest{
		"resolution": {Resolution: 123}, "color": {Color: "sepia"}, "source": {Source: "feeder"}, "paper": {Paper: "napkin"},
	} {
		if _, err := f.hub.StartScan(ctx, req); !errors.Is(err, hub.ErrInvalid) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	f.paperless.FailImport = true
	sc, err := f.hub.StartScan(ctx, hub.ScanRequest{Resolution: 75})
	if err != nil {
		t.Fatal(err)
	}
	sc, err = f.hub.FileScan(ctx, sc.ID, hub.FileRequest{Wait: true})
	if err != nil || sc.Paperless.Status != "failure" || sc.Paperless.Message == "" {
		t.Errorf("failed import not reported: %+v, %v", sc.Paperless, err)
	}
	if list, _ := f.hub.Scans(0); len(list) != 1 {
		t.Errorf("%d scans listed", len(list))
	}
}

func TestJobIsFollowedToCompletion(t *testing.T) {
	f := newFixture(t, nil)
	res, err := f.hub.Print(context.Background(), hub.PrintRequest{Source: hub.Source{Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := f.hub.Subscribe()
	defer cancel()
	deadline := time.After(10 * time.Second)
	for {
		if h := f.hub.History(1); h[0].ID == res.ID && h[0].State == "completed" {
			return
		}
		select {
		case <-events:
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatalf("job never reached completed: %+v", f.hub.History(1))
		}
	}
}
