// Package testutil provides fake devices for tests: an IPP printer, an eSCL
// scanner and a Paperless-ngx instance. They speak the real protocols over HTTP,
// so the code under test runs exactly as it does against hardware.
package testutil

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenPrinting/goipp"
)

// PrintedJob is a job the fake printer received.
type PrintedJob struct {
	ID       int
	Name     string
	Format   string
	Document []byte
	Attrs    map[string]string // job template attributes, as text
	Canceled bool
}

// Printer is a fake IPP printer.
type Printer struct {
	*httptest.Server

	// Formats is document-format-supported. Set it before the first request.
	Formats []string
	// NotAccepting makes the printer refuse jobs.
	NotAccepting bool
	// Busy rejects Print-Job even when the printer advertises accepting jobs.
	Busy bool
	// PageRanges is the page-ranges-supported attribute.
	PageRanges bool
	// Down makes the printer answer every request with an HTTP error, as a
	// printer in a broken state or a wrong address would.
	Down atomic.Bool

	mu       sync.Mutex
	jobs     []*PrintedJob
	requests int
}

// NewRasterPrinter returns a printer like an AirPrint inkjet: it takes raster
// and JPEG, but no PDF.
func NewRasterPrinter(t *testing.T) *Printer {
	return newPrinter(t, []string{"application/octet-stream", "image/jpeg", "image/urf", "image/pwg-raster"})
}

// NewPDFPrinter returns a printer (or print server) that takes PDF.
func NewPDFPrinter(t *testing.T) *Printer {
	return newPrinter(t, []string{"application/octet-stream", "application/pdf", "image/jpeg", "image/pwg-raster"})
}

func newPrinter(t *testing.T, formats []string) *Printer {
	p := &Printer{Formats: formats}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Close)
	return p
}

// URI returns the printer's ipp-over-http address.
func (p *Printer) URI() string { return p.URL + "/ipp/print" }

// Jobs returns the jobs received so far.
func (p *Printer) Jobs() []*PrintedJob {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*PrintedJob(nil), p.jobs...)
}

// Requests returns how many IPP requests the printer received.
func (p *Printer) Requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

func keywords(values ...string) []goipp.Value {
	out := make([]goipp.Value, len(values))
	for i, v := range values {
		out[i] = goipp.String(v)
	}
	return out
}

func attr(name string, tag goipp.Tag, values ...goipp.Value) goipp.Attribute {
	return goipp.MakeAttr(name, tag, values[0], values[1:]...)
}

func (p *Printer) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests++
	p.mu.Unlock()
	if p.Down.Load() {
		http.Error(w, "printer error", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), goipp.ContentType) {
		http.Error(w, "not an IPP request", http.StatusBadRequest)
		return
	}
	var req goipp.Message
	// The document follows the IPP message in the same body; DecodeEx stops
	// reading right after the message.
	if err := req.DecodeEx(r.Body, goipp.DecoderOptions{}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	resp.Operation.Add(attr("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	resp.Operation.Add(attr("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	op := map[string]goipp.Values{}
	for _, a := range req.Operation {
		op[a.Name] = a.Values
	}

	switch goipp.Op(req.Code) {
	case goipp.OpGetPrinterAttributes:
		p.printerAttributes(resp)

	case goipp.OpValidateJob:

	case goipp.OpPrintJob:
		doc, _ := io.ReadAll(r.Body)
		if p.Busy {
			resp.Code = goipp.Code(goipp.StatusErrorBusy)
			resp.Operation.Add(attr("status-message", goipp.TagText, goipp.String("Not allowed to print")))
			break
		}
		if p.NotAccepting {
			resp.Code = goipp.Code(goipp.StatusErrorNotAcceptingJobs)
			break
		}
		job := &PrintedJob{Document: doc, Attrs: map[string]string{}}
		if v := op["job-name"]; len(v) > 0 {
			job.Name = v[0].V.String()
		}
		if v := op["document-format"]; len(v) > 0 {
			job.Format = v[0].V.String()
		}
		for _, a := range req.Job {
			job.Attrs[a.Name] = a.Values.String()
		}
		p.mu.Lock()
		job.ID = len(p.jobs) + 1
		p.jobs = append(p.jobs, job)
		p.mu.Unlock()
		resp.Job.Add(attr("job-id", goipp.TagInteger, goipp.Integer(job.ID)))
		resp.Job.Add(attr("job-uri", goipp.TagURI, goipp.String(p.URI()+"/jobs/1")))
		resp.Job.Add(attr("job-state", goipp.TagEnum, goipp.Integer(5)))
		resp.Job.Add(attr("job-state-reasons", goipp.TagKeyword, goipp.String("job-printing")))

	case goipp.OpGetJobAttributes, goipp.OpCancelJob:
		id := 0
		if v := op["job-id"]; len(v) > 0 {
			if n, ok := v[0].V.(goipp.Integer); ok {
				id = int(n)
			}
		}
		p.mu.Lock()
		var job *PrintedJob
		if id >= 1 && id <= len(p.jobs) {
			job = p.jobs[id-1]
		}
		if job != nil && goipp.Op(req.Code) == goipp.OpCancelJob {
			job.Canceled = true
		}
		p.mu.Unlock()
		if job == nil {
			resp.Code = goipp.Code(goipp.StatusErrorNotFound)
			break
		}
		if goipp.Op(req.Code) == goipp.OpGetJobAttributes {
			p.jobAttributes(&resp.Job, job)
		}

	case goipp.OpGetJobs:
		p.mu.Lock()
		for _, job := range p.jobs {
			var g goipp.Attributes
			p.jobAttributes(&g, job)
			resp.Groups = append(resp.Groups, goipp.Group{Tag: goipp.TagJobGroup, Attrs: g})
		}
		p.mu.Unlock()
		if len(resp.Groups) > 0 {
			resp.Groups = append(goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: resp.Operation}}, resp.Groups...)
		}

	case goipp.OpIdentifyPrinter:

	default:
		resp.Code = goipp.Code(goipp.StatusErrorOperationNotSupported)
	}

	var buf bytes.Buffer
	if err := resp.Encode(&buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", goipp.ContentType)
	_, _ = w.Write(buf.Bytes())
}

func (p *Printer) jobAttributes(g *goipp.Attributes, job *PrintedJob) {
	state, reason := 9, "job-completed-successfully"
	if job.Canceled {
		state, reason = 7, "job-canceled-by-user"
	}
	g.Add(attr("job-id", goipp.TagInteger, goipp.Integer(job.ID)))
	g.Add(attr("job-name", goipp.TagName, goipp.String(job.Name)))
	g.Add(attr("job-state", goipp.TagEnum, goipp.Integer(state)))
	g.Add(attr("job-state-reasons", goipp.TagKeyword, goipp.String(reason)))
	g.Add(attr("job-originating-user-name", goipp.TagName, goipp.String("platen")))
	g.Add(attr("job-impressions-completed", goipp.TagInteger, goipp.Integer(1)))
}

func (p *Printer) printerAttributes(resp *goipp.Message) {
	a := &resp.Printer
	a.Add(attr("printer-name", goipp.TagName, goipp.String("Test printer")))
	a.Add(attr("printer-info", goipp.TagText, goipp.String("Test printer")))
	a.Add(attr("printer-make-and-model", goipp.TagText, goipp.String("Platen Test Inkjet")))
	a.Add(attr("printer-state", goipp.TagEnum, goipp.Integer(3)))
	a.Add(attr("printer-state-reasons", goipp.TagKeyword, goipp.String("none")))
	a.Add(attr("printer-is-accepting-jobs", goipp.TagBoolean, goipp.Boolean(!p.NotAccepting)))
	a.Add(attr("queued-job-count", goipp.TagInteger, goipp.Integer(0)))
	a.Add(attr("document-format-supported", goipp.TagMimeType, keywords(p.Formats...)...))
	a.Add(attr("print-color-mode-supported", goipp.TagKeyword, keywords("color", "monochrome", "auto")...))
	a.Add(attr("sides-supported", goipp.TagKeyword, keywords("one-sided", "two-sided-long-edge", "two-sided-short-edge")...))
	a.Add(attr("print-quality-supported", goipp.TagEnum, goipp.Integer(3), goipp.Integer(4), goipp.Integer(5)))
	a.Add(attr("media-supported", goipp.TagKeyword, keywords("iso_a4_210x297mm", "na_letter_8.5x11in", "iso_a5_148x210mm", "na_index-4x6_4x6in")...))
	a.Add(attr("media-default", goipp.TagKeyword, goipp.String("iso_a4_210x297mm")))
	a.Add(attr("media-ready", goipp.TagKeyword, goipp.String("iso_a4_210x297mm")))
	a.Add(attr("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 99}))
	a.Add(attr("page-ranges-supported", goipp.TagBoolean, goipp.Boolean(p.PageRanges)))
	a.Add(attr("print-scaling-supported", goipp.TagKeyword, keywords("auto", "fit", "fill", "none")...))
	a.Add(attr("job-creation-attributes-supported", goipp.TagKeyword,
		keywords("copies", "sides", "media", "print-quality", "printer-resolution", "print-color-mode", "page-ranges", "job-name")...))
	a.Add(attr("pwg-raster-document-resolution-supported", goipp.TagResolution, goipp.Resolution{Xres: 600, Yres: 600, Units: goipp.UnitsDpi}))
	a.Add(attr("pwg-raster-document-type-supported", goipp.TagKeyword, keywords("srgb_8", "sgray_8")...))
	a.Add(attr("pwg-raster-document-sheet-back", goipp.TagKeyword, goipp.String("rotated")))
	a.Add(attr("marker-names", goipp.TagName, keywords("Black", "Cyan", "Magenta", "Yellow")...))
	a.Add(attr("marker-types", goipp.TagKeyword, keywords("ink-cartridge", "ink-cartridge", "ink-cartridge", "ink-cartridge")...))
	a.Add(attr("marker-colors", goipp.TagName, keywords("#000000", "#00CFFF", "#F200FF", "#FFDA00")...))
	a.Add(attr("marker-levels", goipp.TagInteger, goipp.Integer(70), goipp.Integer(50), goipp.Integer(8), goipp.Integer(50)))
}
