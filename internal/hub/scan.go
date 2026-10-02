package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"strings"
	"time"

	"github.com/giovannirco/platen/internal/escl"
	"github.com/giovannirco/platen/internal/paperless"
	"github.com/giovannirco/platen/internal/pdfw"
	"github.com/giovannirco/platen/internal/render"
	"github.com/giovannirco/platen/internal/store"
)

// ScanRequest describes a scan.
type ScanRequest struct {
	// Scanner is the scanner id. Empty means the default scanner.
	Scanner string `json:"scanner,omitempty"`
	// Source is "flatbed" (default when there is one) or "feeder".
	Source string `json:"source,omitempty"`
	// Duplex scans both sides when the source is a feeder that can.
	Duplex bool `json:"duplex,omitempty"`
	// Color is "color" (default) or "gray".
	Color string `json:"color,omitempty"`
	// Resolution in dpi. Defaults to 300.
	Resolution int `json:"resolution,omitempty"`
	// Paper is the area to scan: "a4" (default), "letter", "legal", "a5", "full"
	// or a size in millimetres such as "100x150".
	Paper string `json:"paper,omitempty"`
	// Title names the scan.
	Title string `json:"title,omitempty"`
	Via   string `json:"-"`
}

// paperSizes in millimetres.
var paperSizes = map[string][2]float64{
	"a4": {210, 297}, "a5": {148, 210}, "a6": {105, 148}, "b5": {182, 257},
	"letter": {215.9, 279.4}, "legal": {215.9, 355.6}, "4x6": {101.6, 152.4}, "5x7": {127, 177.8},
}

// settings turns a request into scanner settings and fills the request's defaults.
func scanSettings(req *ScanRequest, caps *escl.Capabilities) (escl.Settings, error) {
	var s escl.Settings
	var in *escl.InputCaps
	switch strings.ToLower(req.Source) {
	case "":
		if caps.Platen != nil {
			req.Source = "flatbed"
		} else {
			req.Source = "feeder"
		}
		return scanSettings(req, caps)
	case "flatbed", "platen", "glass":
		req.Source, s.Source, in = "flatbed", "Platen", caps.Platen
	case "feeder", "adf":
		req.Source, s.Source, in = "feeder", "Feeder", caps.Feeder
		if req.Duplex {
			if caps.FeederDuplex == nil {
				return s, invalid("the feeder can't scan both sides")
			}
			in, s.Duplex = caps.FeederDuplex, true
		}
	default:
		return s, invalid("source %q: use flatbed or feeder", req.Source)
	}
	if in == nil {
		return s, invalid("the scanner has no %s", req.Source)
	}

	switch strings.ToLower(req.Color) {
	case "", "color", "colour":
		req.Color, s.ColorMode = "color", "RGB24"
	case "gray", "grey", "grayscale", "monochrome":
		req.Color, s.ColorMode = "gray", "Grayscale8"
	default:
		return s, invalid("color %q: use color or gray", req.Color)
	}
	if len(in.ColorModes) > 0 && !contains(in.ColorModes, s.ColorMode) {
		return s, invalid("the scanner can't scan in %s", req.Color)
	}

	if req.Resolution == 0 {
		req.Resolution = 300
	}
	if len(in.Resolutions) > 0 {
		ok := false
		for _, r := range in.Resolutions {
			ok = ok || r == req.Resolution
		}
		if !ok {
			return s, invalid("resolution %d dpi: the scanner offers %v", req.Resolution, in.Resolutions)
		}
	}
	s.Resolution = req.Resolution

	// Region, in 1/300 inch.
	paper := strings.ToLower(strings.TrimSpace(req.Paper))
	if paper == "" {
		paper = "a4"
	}
	req.Paper = paper
	w, h := in.MaxWidth, in.MaxHeight
	if paper != "full" {
		mm, ok := paperSizes[paper]
		if !ok {
			var a, b float64
			if n, _ := fmt.Sscanf(paper, "%fx%f", &a, &b); n != 2 || a <= 0 || b <= 0 {
				return s, invalid("paper %q: use a4, letter, legal, a5, full or a size in millimetres such as 100x150", req.Paper)
			}
			mm = [2]float64{a, b}
		}
		w, h = int(mm[0]*300/25.4+0.5), int(mm[1]*300/25.4+0.5)
	}
	if in.MaxWidth > 0 {
		w = min(w, in.MaxWidth)
	}
	if in.MaxHeight > 0 {
		h = min(h, in.MaxHeight)
	}
	s.Width, s.Height = w, h
	// No scan intent is sent: with one, some scanners (Canon PIXMA) crop the
	// picture to what they think the document is instead of the region asked for.
	s.Format = "image/jpeg"
	if len(in.Formats) > 0 && !contains(in.Formats, "image/jpeg") {
		return s, fmt.Errorf("%w: the scanner doesn't deliver JPEG", ErrUnsupported)
	}
	return s, nil
}

// StartScan opens a scan session and scans the first page (flatbed) or every
// page in the feeder.
func (h *Hub) StartScan(ctx context.Context, req ScanRequest) (*store.Scan, error) {
	dev, err := h.scanner(req.Scanner)
	if err != nil {
		return nil, err
	}
	caps, err := dev.capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("scanner %s is not reachable: %w", dev.cfg.ID, err)
	}
	settings, err := scanSettings(&req, caps)
	if err != nil {
		return nil, err
	}
	if !dev.busy.TryLock() {
		return nil, ErrScannerBusy
	}
	defer dev.busy.Unlock()
	dev.scanning.Store(true)
	defer dev.scanning.Store(false)

	h.scanMu.Lock()
	sc, err := h.store.CreateScan(store.Scan{
		Scanner: dev.cfg.ID, Title: strings.TrimSpace(req.Title), Source: req.Source, Color: req.Color,
		DPI: req.Resolution, Paper: req.Paper, Via: req.Via,
	})
	h.scanMu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := h.acquire(ctx, dev, sc, settings); err != nil {
		if len(sc.Pages) == 0 {
			_ = h.store.DeleteScan(sc.ID)
		}
		return nil, err
	}
	return sc, nil
}

// ScanNextPage adds a page to an open scan, with the settings it was started with.
func (h *Hub) ScanNextPage(ctx context.Context, id string) (*store.Scan, error) {
	sc, err := h.Scan(id)
	if err != nil {
		return nil, err
	}
	dev, err := h.scanner(sc.Scanner)
	if err != nil {
		return nil, err
	}
	caps, err := dev.capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("scanner %s is not reachable: %w", dev.cfg.ID, err)
	}
	req := ScanRequest{Source: sc.Source, Color: sc.Color, Resolution: sc.DPI, Paper: sc.Paper}
	settings, err := scanSettings(&req, caps)
	if err != nil {
		return nil, err
	}
	if !dev.busy.TryLock() {
		return nil, ErrScannerBusy
	}
	defer dev.busy.Unlock()
	dev.scanning.Store(true)
	defer dev.scanning.Store(false)
	if err := h.acquire(ctx, dev, sc, settings); err != nil {
		return nil, err
	}
	return sc, nil
}

// acquire runs one scan job and stores every page it delivers.
func (h *Hub) acquire(ctx context.Context, dev *scannerDev, sc *store.Scan, settings escl.Settings) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	job, err := dev.client.Start(ctx, settings)
	if err != nil {
		if errors.Is(err, escl.ErrBusy) {
			return ErrScannerBusy
		}
		return err
	}
	defer job.Close(context.WithoutCancel(ctx))

	added := 0
	for {
		body, _, err := job.NextDocument(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if added > 0 {
				break // a feeder that ran empty answers with an error on some scanners
			}
			return err
		}
		data, err := io.ReadAll(io.LimitReader(body, 512<<20))
		body.Close()
		if err != nil {
			return fmt.Errorf("read scanned page: %w", err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("the scanner did not deliver a JPEG image: %w", err)
		}
		// A scan takes seconds, and the person may rename the document or remove
		// a page meanwhile. The page is therefore added to the scan as it is on
		// disk now, not to the copy read before the scanner started.
		h.scanMu.Lock()
		cur, err := h.store.Scan(sc.ID)
		if err == nil {
			_, err = h.store.AddPage(cur, data, cfg.Width, cfg.Height)
		}
		h.scanMu.Unlock()
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: scan %q was deleted while its next page was scanned", ErrNotFound, sc.ID)
		}
		if err != nil {
			return err
		}
		*sc = *cur
		added++
		h.events.publish("scan", sc.ID, sc)
		if settings.Source == "Platen" {
			break // the flatbed holds one page
		}
	}
	if added == 0 {
		return errors.New("the scanner delivered no page (is there a document in the feeder?)")
	}
	return nil
}

// Scan returns a scan session.
func (h *Hub) Scan(id string) (*store.Scan, error) {
	sc, err := h.store.Scan(id)
	if err != nil {
		return nil, fmt.Errorf("%w: scan %q", ErrNotFound, id)
	}
	return sc, nil
}

// Scans lists scan sessions, newest first.
func (h *Hub) Scans(limit int) ([]*store.Scan, error) { return h.store.Scans(limit) }

// RemoveScanPage deletes one page of a scan.
func (h *Hub) RemoveScanPage(id, pageID string) (*store.Scan, error) {
	h.scanMu.Lock()
	defer h.scanMu.Unlock()
	sc, err := h.Scan(id)
	if err != nil {
		return nil, err
	}
	if err := h.store.RemovePage(sc, pageID); err != nil {
		return nil, fmt.Errorf("%w: page %q", ErrNotFound, pageID)
	}
	h.events.publish("scan", sc.ID, sc)
	return sc, nil
}

// MoveScanPage moves a page to a new position (0-based).
func (h *Hub) MoveScanPage(id, pageID string, to int) (*store.Scan, error) {
	h.scanMu.Lock()
	defer h.scanMu.Unlock()
	sc, err := h.Scan(id)
	if err != nil {
		return nil, err
	}
	if err := h.store.MovePage(sc, pageID, to); err != nil {
		return nil, fmt.Errorf("%w: page %q", ErrNotFound, pageID)
	}
	h.events.publish("scan", sc.ID, sc)
	return sc, nil
}

// RenameScan sets the title of a scan.
func (h *Hub) RenameScan(id, title string) (*store.Scan, error) {
	h.scanMu.Lock()
	defer h.scanMu.Unlock()
	sc, err := h.Scan(id)
	if err != nil {
		return nil, err
	}
	sc.Title = strings.TrimSpace(title)
	if err := h.store.SaveScan(sc); err != nil {
		return nil, err
	}
	h.events.publish("scan", sc.ID, sc)
	return sc, nil
}

// DeleteScan removes a scan and its files.
func (h *Hub) DeleteScan(id string) error {
	h.scanMu.Lock()
	defer h.scanMu.Unlock()
	if err := h.store.DeleteScan(id); err != nil {
		return fmt.Errorf("%w: scan %q", ErrNotFound, id)
	}
	h.events.publish("scan.deleted", id, nil)
	return nil
}

// FinishScan builds the document of a scan: a PDF with every page, or a single
// JPEG when format is "jpeg" and the scan has one page.
func (h *Hub) FinishScan(id, format string) (*store.Scan, error) {
	h.scanMu.Lock()
	defer h.scanMu.Unlock()
	sc, err := h.Scan(id)
	if err != nil {
		return nil, err
	}
	if len(sc.Pages) == 0 {
		return nil, invalid("the scan has no pages")
	}
	switch format {
	case "", "pdf":
		format = "pdf"
	case "jpeg", "jpg":
		format = "jpeg"
		if len(sc.Pages) > 1 {
			return nil, invalid("a JPEG holds one page; this scan has %d, so use pdf", len(sc.Pages))
		}
	default:
		return nil, invalid("format %q: use pdf or jpeg", format)
	}
	if sc.Document != nil && sc.Document.Format == format {
		return sc, nil
	}
	path, err := h.store.DocumentPath(sc, format)
	if err != nil {
		return nil, err
	}
	if format == "jpeg" {
		data, err := os.ReadFile(sc.Pages[0].File)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, data, 0o640); err != nil {
			return nil, err
		}
		sc.Document = &store.ScanDoc{File: path, Format: "jpeg", Bytes: int64(len(data)), Pages: 1}
	} else {
		pages := make([]pdfw.Page, 0, len(sc.Pages))
		for _, p := range sc.Pages {
			data, err := os.ReadFile(p.File)
			if err != nil {
				return nil, err
			}
			pages = append(pages, pdfw.Page{JPEG: data, DPI: sc.DPI})
		}
		var buf bytes.Buffer
		info := pdfw.Info{Title: sc.Title, Creator: "Platen", Created: sc.CreatedAt}
		if err := pdfw.Write(&buf, pages, info); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
			return nil, err
		}
		sc.Document = &store.ScanDoc{File: path, Format: "pdf", Bytes: int64(buf.Len()), Pages: len(pages)}
	}
	if err := h.store.SaveScan(sc); err != nil {
		return nil, err
	}
	h.events.publish("scan", sc.ID, sc)
	return sc, nil
}

// ScanFilename suggests a file name for the finished document of a scan.
func ScanFilename(sc *store.Scan) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '"' || r < 0x20:
			return '-'
		}
		return r
	}, strings.TrimSpace(sc.Title))
	if base == "" {
		base = "scan-" + sc.CreatedAt.Format("2006-01-02-150405")
	}
	if sc.Document != nil && sc.Document.Format == "jpeg" {
		return base + ".jpg"
	}
	return base + ".pdf"
}

// PageImage returns a page of a scan as JPEG. maxSide > 0 returns a smaller
// preview whose longer side is at most maxSide pixels.
func (h *Hub) PageImage(id, pageID string, maxSide int) ([]byte, error) {
	sc, err := h.Scan(id)
	if err != nil {
		return nil, err
	}
	for _, p := range sc.Pages {
		if p.ID != pageID {
			continue
		}
		if maxSide <= 0 || max(p.Width, p.Height) <= maxSide {
			return os.ReadFile(p.File)
		}
		// One cached preview per page is enough for the UI; other sizes are made on the fly.
		const cached = 480
		if maxSide == cached {
			if data, err := os.ReadFile(store.ThumbPath(p.File)); err == nil {
				return data, nil
			}
		}
		raw, err := os.ReadFile(p.File)
		if err != nil {
			return nil, err
		}
		img, err := jpeg.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, render.Thumbnail(img, maxSide), &jpeg.Options{Quality: 82}); err != nil {
			return nil, err
		}
		if maxSide == cached {
			_ = os.WriteFile(store.ThumbPath(p.File), buf.Bytes(), 0o640)
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("%w: page %q", ErrNotFound, pageID)
}

// PagePreview decodes a small version of a page, for callers that want pixels.
func (h *Hub) PagePreview(id, pageID string, maxSide int) (image.Image, error) {
	data, err := h.PageImage(id, pageID, maxSide)
	if err != nil {
		return nil, err
	}
	return jpeg.Decode(bytes.NewReader(data))
}

// ---- Paperless ---------------------------------------------------------------

// FileRequest says how to file a document in Paperless-ngx. Names that don't
// exist yet are created.
type FileRequest struct {
	Title         string   `json:"title,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Correspondent string   `json:"correspondent,omitempty"`
	DocumentType  string   `json:"document_type,omitempty"`
	// Created is the document's own date, as YYYY-MM-DD.
	Created string `json:"created,omitempty"`
	// Wait blocks until Paperless has imported the document (up to two minutes).
	Wait bool `json:"wait,omitempty"`
}

// PaperlessEnabled reports whether a Paperless instance is configured.
func (h *Hub) PaperlessEnabled() bool { return h.paperless != nil }

// PaperlessMeta lists the tags, correspondents and document types of the instance.
type PaperlessMeta struct {
	URL            string            `json:"url"`
	Tags           []paperless.Named `json:"tags"`
	Correspondents []paperless.Named `json:"correspondents"`
	DocumentTypes  []paperless.Named `json:"document_types"`
}

// PaperlessMeta reads the lists used to file a document.
func (h *Hub) PaperlessMeta(ctx context.Context) (*PaperlessMeta, error) {
	if h.paperless == nil {
		return nil, ErrNoPaperless
	}
	m := &PaperlessMeta{URL: h.cfg.Paperless.PublicURL}
	var err error
	if m.Tags, err = h.paperless.Tags(ctx); err != nil {
		return nil, err
	}
	if m.Correspondents, err = h.paperless.Correspondents(ctx); err != nil {
		return nil, err
	}
	if m.DocumentTypes, err = h.paperless.DocumentTypes(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// PaperlessSearch finds documents in Paperless.
func (h *Hub) PaperlessSearch(ctx context.Context, query string, limit int) ([]PaperlessDocument, int, error) {
	if h.paperless == nil {
		return nil, 0, ErrNoPaperless
	}
	docs, total, err := h.paperless.Search(ctx, query, limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]PaperlessDocument, len(docs))
	for i, d := range docs {
		out[i] = PaperlessDocument{Document: d, URL: paperless.DocumentURL(h.cfg.Paperless.PublicURL, d.ID)}
	}
	return out, total, nil
}

// PaperlessDocument is a search hit with its address in the Paperless web interface.
type PaperlessDocument struct {
	paperless.Document
	URL string `json:"url"`
}

// FileScan sends the finished document of a scan to Paperless-ngx.
func (h *Hub) FileScan(ctx context.Context, id string, req FileRequest) (*store.Scan, error) {
	if h.paperless == nil {
		return nil, ErrNoPaperless
	}
	sc, err := h.FinishScan(id, "")
	if err != nil {
		return nil, err
	}
	up := paperless.Upload{Title: orDefault(strings.TrimSpace(req.Title), sc.Title)}
	if req.Created != "" {
		if up.Created, err = time.Parse("2006-01-02", req.Created); err != nil {
			return nil, invalid("created %q: use YYYY-MM-DD", req.Created)
		}
	}
	for _, name := range req.Tags {
		if strings.TrimSpace(name) == "" {
			continue
		}
		tid, err := h.paperless.EnsureNamed(ctx, "tags", name)
		if err != nil {
			return nil, fmt.Errorf("tag %q: %w", name, err)
		}
		up.Tags = append(up.Tags, tid)
	}
	if req.Correspondent != "" {
		if up.Correspondent, err = h.paperless.EnsureNamed(ctx, "correspondents", req.Correspondent); err != nil {
			return nil, fmt.Errorf("correspondent %q: %w", req.Correspondent, err)
		}
	}
	if req.DocumentType != "" {
		if up.DocumentType, err = h.paperless.EnsureNamed(ctx, "document_types", req.DocumentType); err != nil {
			return nil, fmt.Errorf("document type %q: %w", req.DocumentType, err)
		}
	}
	f, err := os.Open(sc.Document.File)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if up.Title != "" && sc.Title == "" {
		sc.Title = up.Title
	}
	up.Filename, up.Data = ScanFilename(sc), f
	task, err := h.paperless.Upload(ctx, up)
	if err != nil {
		return nil, err
	}
	filed := store.Filed{TaskID: task, Status: "pending", At: time.Now()}
	// save stores a copy of the filing state, so the scan handed back to the
	// caller is never changed by the goroutine that follows the import. An
	// untitled scan takes the title it was filed under.
	save := func(f store.Filed) *store.Scan {
		h.scanMu.Lock()
		defer h.scanMu.Unlock()
		cur, err := h.store.Scan(id)
		if err != nil {
			return nil
		}
		cur.Paperless = &f
		if cur.Title == "" {
			cur.Title = up.Title
		}
		_ = h.store.SaveScan(cur)
		h.events.publish("scan", cur.ID, cur)
		return cur
	}
	follow := func(ctx context.Context) *store.Scan {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		f := filed
		t, err := h.paperless.WaitTask(ctx, task)
		switch {
		case err != nil && t != nil && t.Status == "failure":
			f.Status, f.Message = "failure", t.Result
		case err != nil:
			f.Status, f.Message = "unknown", err.Error()
		default:
			f.Status, f.DocumentID, f.Message = "success", t.DocumentID, t.Result
			if t.DocumentID > 0 {
				f.URL = paperless.DocumentURL(h.cfg.Paperless.PublicURL, t.DocumentID)
			}
		}
		return save(f)
	}
	if cur := save(filed); cur != nil {
		sc = cur
	}
	if req.Wait {
		// The import is followed to its end even when the caller goes away, so
		// the stored state says what happened in Paperless, not "canceled".
		if cur := follow(context.WithoutCancel(ctx)); cur != nil {
			sc = cur
		}
	} else {
		go follow(context.Background())
	}
	return sc, nil
}
