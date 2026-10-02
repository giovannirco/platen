package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	// Decoders for the picture formats Platen accepts.
	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/giovannirco/platen/internal/ipp"
	"github.com/giovannirco/platen/internal/pdfw"
	"github.com/giovannirco/platen/internal/raster"
	"github.com/giovannirco/platen/internal/render"
	"github.com/giovannirco/platen/internal/store"
)

// PrintRequest describes a print job.
type PrintRequest struct {
	// Printer is the printer id. Empty means the default printer.
	Printer string `json:"printer,omitempty"`
	Source  Source `json:"source"`
	// Title names the job. Defaults to the document's name.
	Title string `json:"title,omitempty"`
	// Copies defaults to 1.
	Copies int `json:"copies,omitempty"`
	// Duplex is "off" (default), "long-edge" or "short-edge".
	Duplex string `json:"duplex,omitempty"`
	// Color is "auto" (default), "color" or "monochrome".
	Color string `json:"color,omitempty"`
	// Quality is "draft", "normal" (default) or "high".
	Quality string `json:"quality,omitempty"`
	// Media is a paper size: a PWG name (iso_a4_210x297mm) or a short name
	// (a4, letter, legal, a5, 4x6, 5x7). Defaults to what the printer has loaded.
	Media string `json:"media,omitempty"`
	// Pages selects pages, e.g. "1-3,5" or "2-". Empty prints everything.
	Pages string `json:"pages,omitempty"`
	// Fill makes a picture cover the whole sheet, cropping what doesn't fit.
	Fill bool `json:"fill,omitempty"`
	// Confirm acknowledges that the job needs more sheets than the configured threshold.
	Confirm bool `json:"confirm,omitempty"`
	// DryRun does everything except printing: it reports pages and sheets.
	DryRun bool `json:"dry_run,omitempty"`
	// Via records who asked: web, api or mcp.
	Via string `json:"-"`
	// Dump, when set, receives the exact bytes that would be sent to the printer
	// and nothing is printed. It is used by "platen print -dump" and by tests.
	Dump io.Writer `json:"-"`
}

// PrintResult reports what was (or would be) printed.
type PrintResult struct {
	// ID identifies the job in Platen's history. Empty for a dry run.
	ID           string `json:"id,omitempty"`
	Printer      string `json:"printer"`
	PrinterJobID int    `json:"printer_job_id,omitempty"`
	Title        string `json:"title"`
	// DocumentPages is the page count of the whole document, Pages what is printed.
	DocumentPages int    `json:"document_pages"`
	Pages         int    `json:"pages"`
	Copies        int    `json:"copies"`
	Sheets        int    `json:"sheets"`
	Duplex        string `json:"duplex"`
	Color         string `json:"color"`
	Quality       string `json:"quality"`
	Media         string `json:"media"`
	// Format is what Platen sends to the printer.
	Format  string `json:"format"`
	State   string `json:"state"`
	DryRun  bool   `json:"dry_run,omitempty"`
	Message string `json:"message"`
}

// ConfirmationRequired is returned when a job needs more paper than the
// configured threshold and the request did not confirm it.
type ConfirmationRequired struct {
	Printer string `json:"printer"`
	Title   string `json:"title"`
	Pages   int    `json:"pages"`
	Copies  int    `json:"copies"`
	Sheets  int    `json:"sheets"`
	Limit   int    `json:"confirm_above_sheets"`
}

func (e *ConfirmationRequired) Error() string {
	return fmt.Sprintf("printing %q on %s takes %d sheet(s) of paper (%d page(s), %d cop%s); confirm to go ahead",
		e.Title, e.Printer, e.Sheets, e.Pages, e.Copies, map[bool]string{true: "y", false: "ies"}[e.Copies == 1])
}

var mediaAliases = map[string]string{
	"a3": "iso_a3_297x420mm", "a4": "iso_a4_210x297mm", "a5": "iso_a5_148x210mm", "a6": "iso_a6_105x148mm",
	"b5": "jis_b5_182x257mm", "letter": "na_letter_8.5x11in", "legal": "na_legal_8.5x14in",
	"4x6": "na_index-4x6_4x6in", "5x7": "na_5x7_5x7in", "10x15": "na_index-4x6_4x6in",
}

// pageSource renders the pages of a document for a sheet.
type pageSource interface {
	Pages() int
	Render(index int, t render.Target) (image.Image, error)
	Close()
}

type pdfSource struct{ pdf *render.PDF }

func (s pdfSource) Pages() int { return s.pdf.Pages() }
func (s pdfSource) Render(i int, t render.Target) (image.Image, error) {
	t.Margin = 0
	return s.pdf.RenderPage(i, t)
}
func (s pdfSource) Close() { s.pdf.Close() }

type imageSource struct{ img image.Image }

func (s imageSource) Pages() int { return 1 }
func (s imageSource) Render(_ int, t render.Target) (image.Image, error) {
	return render.FitImage(s.img, t), nil
}
func (s imageSource) Close() {}

type textSource struct {
	text   string
	layout *render.TextLayout
	target render.Target
}

func (s *textSource) Pages() int { return s.layout.Pages() }
func (s *textSource) Render(i int, _ render.Target) (image.Image, error) {
	return s.layout.RenderPage(i)
}
func (s *textSource) Close() {}

// plan is a print job that has been checked and is ready to send.
type plan struct {
	dev   *printerDev
	attrs *ipp.Printer
	doc   *document
	title string

	source   pageSource
	total    int   // pages in the document
	pages    []int // 0-based pages to print, in order
	media    ipp.Media
	sides    string
	duplex   string
	gray     bool
	color    string
	quality  string
	copies   int
	sheets   int
	strategy string // passthrough, raster, pdf, jpeg
	format   string
	native   int // resolution on the wire
	scale    int // native / render resolution
	target   render.Target
	ranges   [][2]int
}

func (pl *plan) close() {
	if pl.source != nil {
		pl.source.Close()
	}
}

// Print checks, prepares and submits a print job.
func (h *Hub) Print(ctx context.Context, req PrintRequest) (*PrintResult, error) {
	dev, err := h.printer(req.Printer)
	if err != nil {
		return nil, err
	}
	attrs, err := dev.attributes(ctx, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("printer %s is not reachable: %w", dev.cfg.ID, err)
	}
	doc, err := h.load(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	pl, err := h.plan(ctx, dev, attrs, doc, req)
	if err != nil {
		return nil, err
	}
	defer pl.close()

	res := &PrintResult{
		Printer: dev.cfg.ID, Title: pl.title, DocumentPages: pl.total, Pages: len(pl.pages),
		Copies: pl.copies, Sheets: pl.sheets, Duplex: pl.duplex, Color: pl.color, Quality: pl.quality,
		Media: pl.media.Name, Format: pl.format, DryRun: req.DryRun,
	}
	lim := h.cfg.Limits
	if lim.MaxSheets > 0 && pl.sheets > lim.MaxSheets {
		return nil, fmt.Errorf("%w: the job needs %d sheets, the limit is %d (limits.max_sheets)", ErrLimitExceeded, pl.sheets, lim.MaxSheets)
	}
	if req.Dump != nil {
		if err := h.encode(ctx, pl, req.Dump); err != nil {
			return nil, err
		}
		res.State, res.DryRun = "not-printed", true
		res.Message = fmt.Sprintf("Wrote %q as %s (%d page(s)); nothing was printed.", pl.title, pl.format, len(pl.pages))
		return res, nil
	}
	if req.DryRun {
		res.State = "not-printed"
		res.Message = fmt.Sprintf("Dry run: %q would print %d page(s) on %d sheet(s) of %s, sent as %s.",
			pl.title, len(pl.pages)*pl.copies, pl.sheets, pl.media.Label(), pl.format)
		return res, nil
	}
	if lim.ConfirmAboveSheets >= 0 && pl.sheets > lim.ConfirmAboveSheets && !req.Confirm {
		return nil, &ConfirmationRequired{Printer: dev.cfg.Name, Title: pl.title, Pages: len(pl.pages), Copies: pl.copies, Sheets: pl.sheets, Limit: lim.ConfirmAboveSheets}
	}
	if !attrs.AcceptingJobs {
		return nil, fmt.Errorf("%s is not accepting jobs (%s)", dev.cfg.Name, orDefault(strings.Join(attrs.StateReasons, ", "), attrs.State))
	}

	job, err := h.submit(ctx, pl)
	if err != nil {
		return nil, err
	}
	rec := store.PrintJob{
		ID: store.NewID("job"), Printer: dev.cfg.ID, PrinterJob: job.ID, Title: pl.title, Source: doc.from,
		Format: pl.format, Pages: len(pl.pages), Copies: pl.copies, Sheets: pl.sheets, Duplex: pl.duplex,
		Color: pl.color, Quality: pl.quality, Media: pl.media.Name, State: job.State, Via: req.Via,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := h.store.AddJob(rec); err != nil {
		h.log.Warn("could not record print job", "error", err)
	}
	h.events.publish(Event{Type: "job", ID: rec.ID, Data: rec})
	go h.watchJob(dev, rec.ID, job.ID)

	res.ID, res.PrinterJobID, res.State = rec.ID, job.ID, job.State
	res.Message = fmt.Sprintf("Sent %q to %s: %d page(s) on %d sheet(s) of %s (printer job %d, %s).",
		pl.title, dev.cfg.Name, len(pl.pages)*pl.copies, pl.sheets, pl.media.Label(), job.ID, job.State)
	return res, nil
}

// plan validates the request against the printer and decides how to send the document.
func (h *Hub) plan(ctx context.Context, dev *printerDev, attrs *ipp.Printer, doc *document, req PrintRequest) (*plan, error) {
	pl := &plan{dev: dev, attrs: attrs, doc: doc, title: orDefault(strings.TrimSpace(req.Title), doc.name)}

	// Copies.
	pl.copies = max(req.Copies, 1)
	if pl.copies > h.cfg.Limits.MaxCopies {
		return nil, fmt.Errorf("%w: %d copies requested, the limit is %d (limits.max_copies)", ErrLimitExceeded, pl.copies, h.cfg.Limits.MaxCopies)
	}
	if attrs.MaxCopies > 0 && pl.copies > attrs.MaxCopies {
		return nil, invalid("the printer prints at most %d copies per job", attrs.MaxCopies)
	}

	// Paper.
	name := strings.ToLower(strings.TrimSpace(req.Media))
	if alias, ok := mediaAliases[name]; ok {
		name = alias
	}
	if name == "" {
		// media-default is what the printer is set up for. media-ready is only a
		// fallback: print servers such as CUPS list every size there.
		name = attrs.MediaDefault
		if name == "" && len(attrs.MediaReady) > 0 {
			name = attrs.MediaReady[0]
		}
		if name == "" {
			name = "iso_a4_210x297mm"
		}
	}
	media, ok := ipp.ParseMedia(name)
	if !ok {
		return nil, invalid("paper size %q: use a4, letter, legal, a5, 4x6, 5x7 or a PWG name such as iso_a4_210x297mm", req.Media)
	}
	if len(attrs.Media) > 0 && !contains(attrs.Media, media.Name) {
		return nil, invalid("the printer has no paper size %q (it offers: %s)", media.Name, joinMax(attrs.Media, 8))
	}
	pl.media = media

	// Sides.
	switch strings.ToLower(req.Duplex) {
	case "", "off", "none", "one-sided", "false":
		pl.sides, pl.duplex = "one-sided", "off"
	case "long-edge", "long", "two-sided-long-edge", "true", "on":
		pl.sides, pl.duplex = "two-sided-long-edge", "long-edge"
	case "short-edge", "short", "two-sided-short-edge":
		pl.sides, pl.duplex = "two-sided-short-edge", "short-edge"
	default:
		return nil, invalid("duplex %q: use off, long-edge or short-edge", req.Duplex)
	}
	if pl.duplex != "off" && !contains(attrs.Sides, pl.sides) {
		return nil, invalid("the printer can't print two-sided (%s)", pl.duplex)
	}

	// Colour.
	switch strings.ToLower(req.Color) {
	case "", "auto":
		pl.color = "auto"
	case "color", "colour":
		pl.color = "color"
	case "monochrome", "mono", "gray", "grey", "bw", "black":
		pl.color, pl.gray = "monochrome", true
	default:
		return nil, invalid("color %q: use auto, color or monochrome", req.Color)
	}

	// Quality.
	switch strings.ToLower(req.Quality) {
	case "", "normal":
		pl.quality = "normal"
	case "draft", "high":
		pl.quality = strings.ToLower(req.Quality)
	default:
		return nil, invalid("quality %q: use draft, normal or high", req.Quality)
	}

	// How to deliver the document.
	raster := attrs.Supports("image/pwg-raster") && (!pl.gray || contains(attrs.RasterTypes, "sgray_8") || len(attrs.RasterTypes) == 0)
	if attrs.Supports("image/pwg-raster") && pl.gray && !raster {
		pl.gray, raster = false, true // the printer only takes colour raster; it still prints gray via print-color-mode
	}
	switch {
	case doc.kind == "pdf" && attrs.Supports("application/pdf"):
		pl.strategy, pl.format = "passthrough", "application/pdf"
	case doc.kind == "jpeg" && attrs.Supports("image/jpeg") && pl.duplex == "off" &&
		(attrs.JPEGMaxKOctets == 0 || len(doc.data) <= attrs.JPEGMaxKOctets<<10):
		pl.strategy, pl.format = "passthrough", "image/jpeg"
	case raster:
		pl.strategy, pl.format = "raster", "image/pwg-raster"
	case attrs.Supports("application/pdf"):
		pl.strategy, pl.format = "pdf", "application/pdf"
	case attrs.Supports("image/jpeg") && doc.kind != "pdf" && doc.kind != "text":
		pl.strategy, pl.format = "jpeg", "image/jpeg"
	default:
		return nil, fmt.Errorf("%w: the printer accepts %s, and Platen can't turn a %s document into any of them",
			ErrUnsupported, joinMax(attrs.Formats, 6), doc.kind)
	}

	// Resolution.
	pl.native, pl.scale = 300, 1
	if pl.strategy == "raster" {
		pl.native, pl.scale = pickResolution(attrs.RasterDPI, pl.quality)
	} else if pl.quality == "draft" {
		pl.native = 150
	} else if pl.quality == "high" {
		pl.native = 600
	}
	renderDPI := pl.native / pl.scale
	w, hgt := media.Pixels(renderDPI)
	pl.target = render.Target{Width: w, Height: hgt, Gray: pl.gray, Margin: renderDPI * 5 / 25, Fill: req.Fill} // 5 mm

	// Open the document and count its pages.
	switch doc.kind {
	case "pdf":
		pdf, err := h.renderer.OpenPDF(ctx, doc.data)
		if err != nil {
			if errors.Is(err, render.ErrPassword) {
				return nil, invalid("%v", err)
			}
			return nil, invalid("the PDF can't be opened: %v", err)
		}
		pl.source = pdfSource{pdf}
	case "jpeg", "image":
		if pl.strategy != "passthrough" {
			img, _, err := image.Decode(bytes.NewReader(doc.data))
			if err != nil {
				return nil, invalid("the picture can't be read: %v", err)
			}
			pl.source = imageSource{img}
		}
	case "text":
		t := pl.target
		t.Margin = renderDPI * 12 / 25 // 12 mm
		layout, err := render.LayoutText(string(doc.data), t, renderDPI, render.TextOptions{})
		if err != nil {
			return nil, invalid("the text can't be laid out: %v", err)
		}
		pl.source = &textSource{layout: layout, target: t}
	}
	pl.total = 1
	if pl.source != nil {
		pl.total = pl.source.Pages()
	}
	if pl.total == 0 {
		pl.close()
		return nil, invalid("the document has no pages")
	}

	// Page selection.
	pages, ranges, err := parsePages(req.Pages, pl.total)
	if err != nil {
		pl.close()
		return nil, err
	}
	pl.pages, pl.ranges = pages, ranges
	if pl.strategy == "passthrough" && len(pages) != pl.total {
		// The printer has to drop pages itself; if it can't, Platen rasterizes instead.
		switch {
		case attrs.PageRanges && sort.IntsAreSorted(pages):
		case raster && doc.kind == "pdf":
			pl.strategy, pl.format = "raster", "image/pwg-raster"
			pl.native, pl.scale = pickResolution(attrs.RasterDPI, pl.quality)
			renderDPI = pl.native / pl.scale
			w, hgt = media.Pixels(renderDPI)
			pl.target = render.Target{Width: w, Height: hgt, Gray: pl.gray, Fill: req.Fill}
		default:
			pl.close()
			return nil, fmt.Errorf("%w: this printer can't print a page selection", ErrUnsupported)
		}
	}
	if pl.strategy == "jpeg" && len(pl.pages) > 1 {
		pl.close()
		return nil, fmt.Errorf("%w: this printer takes one picture per job", ErrUnsupported)
	}

	perSheet := 1
	if pl.duplex != "off" {
		perSheet = 2
	}
	pl.sheets = (len(pl.pages) + perSheet - 1) / perSheet * pl.copies
	return pl, nil
}

// pickResolution chooses the resolution sent to the printer and how much the
// rendered page is enlarged to reach it. Draft renders at about 150 dpi, normal
// at 300 dpi and high at the printer's own resolution.
func pickResolution(supported []int, quality string) (native, scale int) {
	if len(supported) == 0 {
		supported = []int{300}
	}
	sorted := append([]int(nil), supported...)
	sort.Ints(sorted)
	want := map[string]int{"draft": 150, "normal": 300, "high": 1 << 30}[quality]
	if quality == "high" {
		return sorted[len(sorted)-1], 1
	}
	// Prefer a resolution the printer takes directly; otherwise send a multiple.
	for _, r := range sorted {
		if r == want {
			return r, 1
		}
	}
	native = sorted[0]
	for _, r := range sorted {
		if r >= want && r%want == 0 {
			native = r
			break
		}
	}
	scale = max(native/want, 1)
	for native%scale != 0 {
		scale--
	}
	return native, scale
}

// parsePages reads a selection such as "1-3,5,8-" into 0-based page indexes and
// 1-based inclusive ranges.
func parsePages(spec string, total int) (pages []int, ranges [][2]int, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "all" {
		pages = make([]int, total)
		for i := range pages {
			pages[i] = i
		}
		return pages, nil, nil
	}
	seen := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi := part, part
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, hi = strings.TrimSpace(a), strings.TrimSpace(b)
		}
		first, last := 1, total
		if lo != "" {
			if first, err = strconv.Atoi(lo); err != nil {
				return nil, nil, invalid("pages %q: %q is not a page number", spec, lo)
			}
		}
		if hi != "" {
			if last, err = strconv.Atoi(hi); err != nil {
				return nil, nil, invalid("pages %q: %q is not a page number", spec, hi)
			}
		}
		if first < 1 || last < first {
			return nil, nil, invalid("pages %q: %q is not a valid range", spec, part)
		}
		if first > total {
			return nil, nil, invalid("pages %q: the document has only %d page(s)", spec, total)
		}
		last = min(last, total)
		ranges = append(ranges, [2]int{first, last})
		for p := first; p <= last; p++ {
			if !seen[p] {
				seen[p] = true
				pages = append(pages, p-1)
			}
		}
	}
	if len(pages) == 0 {
		return nil, nil, invalid("pages %q selects nothing", spec)
	}
	return pages, ranges, nil
}

func (h *Hub) submit(ctx context.Context, pl *plan) (*ipp.Job, error) {
	jr := ipp.JobRequest{
		Name: pl.title, Format: pl.format, Copies: pl.copies, Sides: pl.sides, Quality: pl.quality, Media: pl.media.Name,
	}
	if pl.color != "auto" {
		jr.ColorMode = pl.color
	}
	// A whole job gets a generous but finite time.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	switch pl.strategy {
	case "passthrough":
		if pl.format == "image/jpeg" {
			jr.Scaling = "auto"
			if pl.target.Fill {
				jr.Scaling = "fill"
			}
		}
		if len(pl.pages) != pl.total {
			jr.PageRanges = pl.ranges
		}
		return pl.dev.client.PrintJob(ctx, pl.attrs, jr, bytes.NewReader(pl.doc.data))
	case "raster":
		// Pages are rendered and sent one at a time, so a long document never
		// sits in memory as a whole.
		jr.ResolutionDPI = pl.native
		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			pw.CloseWithError(h.encode(ctx, pl, pw))
		}()
		job, err := pl.dev.client.PrintJob(ctx, pl.attrs, jr, pr)
		pr.Close() // stops the writer if the printer hung up early
		<-done
		return job, err
	default:
		if pl.strategy == "jpeg" {
			jr.Scaling = "fit"
		}
		var buf bytes.Buffer
		if err := h.encode(ctx, pl, &buf); err != nil {
			return nil, err
		}
		return pl.dev.client.PrintJob(ctx, pl.attrs, jr, &buf)
	}
}

// encode writes the document in the form the printer receives it.
func (h *Hub) encode(ctx context.Context, pl *plan, w io.Writer) error {
	switch pl.strategy {
	case "passthrough":
		_, err := w.Write(pl.doc.data)
		return err
	case "raster":
		return h.writeRaster(ctx, pl, w)
	case "pdf":
		var pages []pdfw.Page
		for _, idx := range pl.pages {
			if err := ctx.Err(); err != nil {
				return err
			}
			img, err := pl.source.Render(idx, pl.target)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
				return err
			}
			pages = append(pages, pdfw.Page{JPEG: buf.Bytes(), DPI: pl.native})
		}
		return pdfw.Write(w, pages, pdfw.Info{Title: pl.title, Creator: "Platen"})
	case "jpeg":
		img, err := pl.source.Render(pl.pages[0], pl.target)
		if err != nil {
			return err
		}
		return jpeg.Encode(w, img, &jpeg.Options{Quality: 92})
	}
	return fmt.Errorf("unknown print strategy %q", pl.strategy)
}

// writeRaster renders the selected pages and writes them as PWG Raster.
func (h *Hub) writeRaster(ctx context.Context, pl *plan, w io.Writer) error {
	rw := raster.NewWriter(w)
	ptsW, ptsH := pl.media.Points()
	duplex := pl.duplex != "off"
	quality := map[string]int{"draft": 3, "normal": 4, "high": 5}[pl.quality]
	for n, idx := range pl.pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		img, err := pl.source.Render(idx, pl.target)
		if err != nil {
			return err
		}
		err = rw.WritePage(raster.Page{
			Image: img, Scale: pl.scale, DPI: pl.native, Gray: pl.gray,
			MediaName: pl.media.Name, PageSizePoints: [2]int{ptsW, ptsH},
			Duplex: duplex, Tumble: pl.duplex == "short-edge", Back: duplex && n%2 == 1,
			SheetBack: raster.SheetBack(pl.attrs.RasterBack), TotalPages: len(pl.pages), Quality: quality,
		})
		if err != nil {
			return err
		}
	}
	return rw.Flush()
}

// watchJob follows a job on the printer and keeps the history up to date.
func (h *Hub) watchJob(dev *printerDev, id string, printerJob int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	delay := time.Second
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay += time.Second
		}
		job, err := dev.client.Job(ctx, printerJob)
		if err != nil {
			// Printers forget finished jobs quickly; after a few misses assume it is done.
			if failures++; failures >= 5 {
				h.updateJob(id, "unknown", "the printer no longer reports this job")
				return
			}
			continue
		}
		failures = 0
		h.updateJob(id, job.State, strings.Join(job.StateReasons, ", "))
		if job.Done() {
			return
		}
	}
}

func (h *Hub) updateJob(id, state, message string) {
	changed := false
	rec, err := h.store.UpdateJob(id, func(j *store.PrintJob) {
		if j.State != state || j.Message != message {
			j.State, j.Message, changed = state, message, true
		}
	})
	if err == nil && changed {
		h.events.publish(Event{Type: "job", ID: id, Data: rec})
	}
}

// CancelJob cancels a job from Platen's history on its printer.
func (h *Hub) CancelJob(ctx context.Context, id string) (store.PrintJob, error) {
	rec, err := h.store.Job(id)
	if err != nil {
		return rec, fmt.Errorf("%w: job %q", ErrNotFound, id)
	}
	dev, err := h.printer(rec.Printer)
	if err != nil {
		return rec, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := dev.client.CancelJob(ctx, rec.PrinterJob); err != nil {
		return rec, err
	}
	h.updateJob(id, "canceled", "canceled on request")
	return h.store.Job(id)
}

// CancelQueueJob cancels a job by the printer's own job number.
func (h *Hub) CancelQueueJob(ctx context.Context, printer string, printerJob int) error {
	dev, err := h.printer(printer)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := dev.client.CancelJob(ctx, printerJob); err != nil {
		return err
	}
	for _, j := range h.store.Jobs(0) {
		if j.Printer == dev.cfg.ID && j.PrinterJob == printerJob {
			h.updateJob(j.ID, "canceled", "canceled on request")
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
