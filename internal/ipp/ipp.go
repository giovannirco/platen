// Package ipp is a small Internet Printing Protocol client.
//
// It covers what a print front end needs: reading a printer's capabilities and
// state, submitting a document, and listing and cancelling jobs. It speaks to real
// printers (IPP Everywhere / AirPrint / Mopria) and to CUPS queues alike. Message
// encoding is done by github.com/OpenPrinting/goipp.
package ipp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/OpenPrinting/goipp"
)

// Client talks to one printer or print queue.
type Client struct {
	printerURI string // value of the printer-uri attribute (ipp:// or ipps://)
	endpoint   string // URL used for the HTTP POST
	http       *http.Client
	user       string
	requestID  atomic.Uint32
}

// Options tune a Client.
type Options struct {
	// InsecureTLS accepts any certificate. Printers usually present a self-signed one.
	InsecureTLS bool
	// UserName is sent as requesting-user-name. Defaults to "platen".
	UserName string
	// HTTPClient replaces the default client (used in tests).
	HTTPClient *http.Client
}

// New returns a client for the printer at uri.
//
// uri may use the ipp, ipps, http or https scheme. The default port for ipp and
// ipps is 631.
func New(uri string, opts Options) (*Client, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("printer uri: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("printer uri %q has no host", uri)
	}
	endpoint, printerURI := *u, *u
	switch u.Scheme {
	case "ipp":
		endpoint.Scheme = "http"
	case "ipps":
		endpoint.Scheme = "https"
	case "http":
		printerURI.Scheme = "ipp"
	case "https":
		printerURI.Scheme = "ipps"
	default:
		return nil, fmt.Errorf("printer uri: unsupported scheme %q", u.Scheme)
	}
	if u.Port() == "" && (u.Scheme == "ipp" || u.Scheme == "ipps") {
		endpoint.Host = net.JoinHostPort(u.Hostname(), "631")
	}
	c := &Client{printerURI: printerURI.String(), endpoint: endpoint.String(), user: opts.UserName, http: opts.HTTPClient}
	if c.user == "" {
		c.user = "platen"
	}
	if c.http == nil {
		c.http = &http.Client{Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: opts.InsecureTLS}, //nolint:gosec // opt-in for self-signed printer certificates
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
		}}
	}
	return c, nil
}

// URI returns the printer URI this client uses.
func (c *Client) URI() string { return c.printerURI }

// Error is an IPP-level failure: the printer answered, but with an error status.
type Error struct {
	Op      string
	Status  goipp.Status
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Op, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Status)
}

func (c *Client) newRequest(op goipp.Op) *goipp.Message {
	m := goipp.NewRequest(goipp.MakeVersion(2, 0), op, c.requestID.Add(1))
	m.Operation.Add(goipp.MakeAttr("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttr("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttr("printer-uri", goipp.TagURI, goipp.String(c.printerURI)))
	m.Operation.Add(goipp.MakeAttr("requesting-user-name", goipp.TagName, goipp.String(c.user)))
	return m
}

// do sends a request, optionally followed by document data, and decodes the reply.
func (c *Client) do(ctx context.Context, name string, msg *goipp.Message, doc io.Reader) (*goipp.Message, error) {
	head, err := msg.EncodeBytes()
	if err != nil {
		return nil, fmt.Errorf("%s: encode: %w", name, err)
	}
	var body io.Reader = bytes.NewReader(head)
	if doc != nil {
		body = io.MultiReader(bytes.NewReader(head), doc)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	switch d := doc.(type) {
	case nil:
		req.ContentLength = int64(len(head))
	case interface{ Len() int }:
		// A document held in memory is sent with its length, which every printer
		// accepts; a streamed one goes out with chunked transfer encoding.
		req.ContentLength = int64(len(head) + d.Len())
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	req.Header.Set("Accept", goipp.ContentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: HTTP %s", name, resp.Status)
	}
	var out goipp.Message
	if err := out.Decode(resp.Body); err != nil {
		return nil, fmt.Errorf("%s: decode reply: %w", name, err)
	}
	if status := goipp.Status(out.Code); status >= 0x0100 {
		return &out, &Error{Op: name, Status: status, Message: index(out.Operation).str("status-message")}
	}
	return &out, nil
}

// Printer is what a printer reports about itself.
type Printer struct {
	Name          string   `json:"name"`
	Info          string   `json:"info,omitempty"`
	MakeAndModel  string   `json:"make_and_model,omitempty"`
	Location      string   `json:"location,omitempty"`
	UUID          string   `json:"uuid,omitempty"`
	Firmware      string   `json:"firmware,omitempty"`
	MoreInfoURL   string   `json:"more_info_url,omitempty"`
	State         string   `json:"state"` // idle, processing or stopped
	StateReasons  []string `json:"state_reasons,omitempty"`
	StateMessage  string   `json:"state_message,omitempty"`
	AcceptingJobs bool     `json:"accepting_jobs"`
	QueuedJobs    int      `json:"queued_jobs"`

	Formats        []string `json:"formats"`                     // document-format-supported
	ColorModes     []string `json:"color_modes,omitempty"`       // print-color-mode-supported
	Sides          []string `json:"sides,omitempty"`             // sides-supported
	Qualities      []string `json:"qualities,omitempty"`         // draft, normal, high
	Media          []string `json:"media,omitempty"`             // media-supported (PWG names)
	MediaDefault   string   `json:"media_default,omitempty"`     // media-default
	MediaReady     []string `json:"media_ready,omitempty"`       // media-ready
	MediaTypes     []string `json:"media_types,omitempty"`       // media-type-supported
	MediaSources   []string `json:"media_sources,omitempty"`     // media-source-supported
	Scaling        []string `json:"scaling,omitempty"`           // print-scaling-supported
	JobAttributes  []string `json:"job_attributes,omitempty"`    // job-creation-attributes-supported
	MaxCopies      int      `json:"max_copies,omitempty"`        // upper bound of copies-supported
	PageRanges     bool     `json:"page_ranges"`                 // page-ranges-supported
	RasterDPI      []int    `json:"raster_dpi,omitempty"`        // pwg-raster-document-resolution-supported
	RasterTypes    []string `json:"raster_types,omitempty"`      // pwg-raster-document-type-supported
	RasterBack     string   `json:"raster_sheet_back,omitempty"` // pwg-raster-document-sheet-back
	JPEGMaxKOctets int      `json:"jpeg_max_k_octets,omitempty"`
	Markers        []Marker `json:"markers,omitempty"`
}

// Marker is an ink or toner supply.
type Marker struct {
	Name  string `json:"name"`
	Type  string `json:"type,omitempty"`
	Color string `json:"color,omitempty"` // #RRGGBB when the printer reports one
	// Level is the fill level in percent, or -1 when unknown.
	Level int `json:"level"`
	// Low is the level under which the printer warns, or -1 when unknown.
	Low int `json:"low"`
}

// Supports reports whether the printer accepts the document format.
func (p *Printer) Supports(format string) bool {
	for _, f := range p.Formats {
		if strings.EqualFold(f, format) {
			return true
		}
	}
	return false
}

// SupportsJobAttribute reports whether a job template attribute may be sent.
// Printers that don't publish the list are assumed to accept the common ones.
func (p *Printer) SupportsJobAttribute(name string) bool {
	if len(p.JobAttributes) == 0 {
		return true
	}
	for _, a := range p.JobAttributes {
		if a == name {
			return true
		}
	}
	return false
}

// PrinterAttributes reads the printer's capabilities and current state.
func (c *Client) PrinterAttributes(ctx context.Context) (*Printer, error) {
	m := c.newRequest(goipp.OpGetPrinterAttributes)
	m.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("all")))
	out, err := c.do(ctx, "Get-Printer-Attributes", m, nil)
	if err != nil {
		return nil, err
	}
	return parsePrinter(index(out.Printer)), nil
}

func parsePrinter(a attrs) *Printer {
	p := &Printer{
		Name:          a.str("printer-name"),
		Info:          a.str("printer-info"),
		MakeAndModel:  a.str("printer-make-and-model"),
		Location:      a.str("printer-location"),
		UUID:          strings.TrimPrefix(a.str("printer-uuid"), "urn:uuid:"),
		Firmware:      a.str("printer-firmware-string-version"),
		MoreInfoURL:   a.str("printer-more-info"),
		StateMessage:  a.str("printer-state-message"),
		AcceptingJobs: a.boolean("printer-is-accepting-jobs"),
		Formats:       a.strs("document-format-supported"),
		ColorModes:    a.strs("print-color-mode-supported"),
		Sides:         a.strs("sides-supported"),
		Media:         a.strs("media-supported"),
		MediaDefault:  a.str("media-default"),
		MediaReady:    dedupe(a.strs("media-ready")),
		MediaTypes:    a.strs("media-type-supported"),
		MediaSources:  a.strs("media-source-supported"),
		Scaling:       a.strs("print-scaling-supported"),
		JobAttributes: a.strs("job-creation-attributes-supported"),
		PageRanges:    a.boolean("page-ranges-supported"),
		RasterTypes:   a.strs("pwg-raster-document-type-supported"),
		RasterBack:    a.str("pwg-raster-document-sheet-back"),
	}
	if n, ok := a.integer("queued-job-count"); ok {
		p.QueuedJobs = n
	}
	switch n, _ := a.integer("printer-state"); n {
	case 3:
		p.State = "idle"
	case 4:
		p.State = "processing"
	case 5:
		p.State = "stopped"
	default:
		p.State = "unknown"
	}
	for _, r := range a.strs("printer-state-reasons") {
		if r != "none" {
			p.StateReasons = append(p.StateReasons, r)
		}
	}
	for _, q := range a.ints("print-quality-supported") {
		switch q {
		case 3:
			p.Qualities = append(p.Qualities, "draft")
		case 4:
			p.Qualities = append(p.Qualities, "normal")
		case 5:
			p.Qualities = append(p.Qualities, "high")
		}
	}
	for _, v := range a["copies-supported"] {
		if r, ok := v.V.(goipp.Range); ok {
			p.MaxCopies = r.Upper
		}
	}
	for _, v := range a["jpeg-k-octets-supported"] {
		if r, ok := v.V.(goipp.Range); ok {
			p.JPEGMaxKOctets = r.Upper
		}
	}
	for _, v := range a["pwg-raster-document-resolution-supported"] {
		if r, ok := v.V.(goipp.Resolution); ok && r.Xres == r.Yres {
			p.RasterDPI = append(p.RasterDPI, r.Xres)
		}
	}
	names, types, colors := a.strs("marker-names"), a.strs("marker-types"), a.strs("marker-colors")
	levels, lows := a.ints("marker-levels"), a.ints("marker-low-levels")
	for i, name := range names {
		mk := Marker{Name: name, Level: -1, Low: -1}
		if i < len(types) {
			mk.Type = types[i]
		}
		if i < len(colors) && strings.HasPrefix(colors[i], "#") {
			mk.Color = colors[i]
		}
		if i < len(levels) && levels[i] >= 0 && levels[i] <= 100 {
			mk.Level = levels[i]
		}
		if i < len(lows) && lows[i] >= 0 && lows[i] <= 100 {
			mk.Low = lows[i]
		}
		p.Markers = append(p.Markers, mk)
	}
	return p
}

// JobRequest describes a document to print.
type JobRequest struct {
	Name   string // job-name
	Format string // document-format (MIME type)

	Copies    int    // 0 or 1 = one copy
	Sides     string // one-sided, two-sided-long-edge, two-sided-short-edge
	ColorMode string // color, monochrome, auto
	Quality   string // draft, normal, high
	Media     string // PWG media name such as iso_a4_210x297mm
	Scaling   string // print-scaling keyword (auto, fit, fill, none)
	// ResolutionDPI sets printer-resolution, needed by some printers for raster jobs.
	ResolutionDPI int
	// PageRanges selects pages (1-based, inclusive). Only sent to printers that
	// support page-ranges; otherwise the caller must drop the pages itself.
	PageRanges [][2]int
}

// Job is a print job as the printer reports it.
type Job struct {
	ID           int       `json:"id"`
	URI          string    `json:"uri,omitempty"`
	Name         string    `json:"name,omitempty"`
	User         string    `json:"user,omitempty"`
	State        string    `json:"state"` // pending, held, processing, stopped, canceled, aborted, completed
	StateReasons []string  `json:"state_reasons,omitempty"`
	Impressions  int       `json:"impressions_completed,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitzero"`
	CompletedAt  time.Time `json:"completed_at,omitzero"`
}

// Done reports whether the job reached a final state.
func (j *Job) Done() bool {
	switch j.State {
	case "canceled", "aborted", "completed":
		return true
	}
	return false
}

func jobTemplate(m *goipp.Message, p *Printer, r JobRequest) {
	supports := func(name string) bool { return p == nil || p.SupportsJobAttribute(name) }
	if r.Copies > 1 && supports("copies") {
		m.Job.Add(goipp.MakeAttr("copies", goipp.TagInteger, goipp.Integer(r.Copies)))
	}
	if r.Sides != "" && supports("sides") {
		m.Job.Add(goipp.MakeAttr("sides", goipp.TagKeyword, goipp.String(r.Sides)))
	}
	if r.ColorMode != "" && supports("print-color-mode") {
		m.Job.Add(goipp.MakeAttr("print-color-mode", goipp.TagKeyword, goipp.String(r.ColorMode)))
	}
	if q := qualityEnum(r.Quality); q != 0 && supports("print-quality") {
		m.Job.Add(goipp.MakeAttr("print-quality", goipp.TagEnum, goipp.Integer(q)))
	}
	if r.Media != "" && supports("media") {
		m.Job.Add(goipp.MakeAttr("media", goipp.TagKeyword, goipp.String(r.Media)))
	}
	if r.Scaling != "" && (p == nil || contains(p.Scaling, r.Scaling)) {
		m.Job.Add(goipp.MakeAttr("print-scaling", goipp.TagKeyword, goipp.String(r.Scaling)))
	}
	if len(r.PageRanges) > 0 && p != nil && p.PageRanges {
		values := make([]goipp.Value, len(r.PageRanges))
		for i, pr := range r.PageRanges {
			values[i] = goipp.Range{Lower: pr[0], Upper: pr[1]}
		}
		m.Job.Add(goipp.MakeAttr("page-ranges", goipp.TagRange, values[0], values[1:]...))
	}
	if r.ResolutionDPI > 0 && supports("printer-resolution") {
		m.Job.Add(goipp.MakeAttr("printer-resolution", goipp.TagResolution,
			goipp.Resolution{Xres: r.ResolutionDPI, Yres: r.ResolutionDPI, Units: goipp.UnitsDpi}))
	}
}

func qualityEnum(q string) int {
	switch q {
	case "draft":
		return 3
	case "normal":
		return 4
	case "high":
		return 5
	}
	return 0
}

// PrintJob submits a document. The printer argument (from PrinterAttributes) lets
// the client leave out job attributes the printer doesn't accept; it may be nil.
func (c *Client) PrintJob(ctx context.Context, printer *Printer, r JobRequest, doc io.Reader) (*Job, error) {
	if doc == nil {
		return nil, errors.New("Print-Job: no document")
	}
	m := c.newRequest(goipp.OpPrintJob)
	if r.Name != "" {
		m.Operation.Add(goipp.MakeAttr("job-name", goipp.TagName, goipp.String(truncate(r.Name, 255))))
	}
	if r.Format != "" {
		m.Operation.Add(goipp.MakeAttr("document-format", goipp.TagMimeType, goipp.String(r.Format)))
	}
	jobTemplate(m, printer, r)
	out, err := c.do(ctx, "Print-Job", m, doc)
	if err != nil {
		return nil, err
	}
	job := parseJob(index(out.Job))
	if job.Name == "" {
		job.Name = r.Name
	}
	return job, nil
}

// ValidateJob asks the printer whether it would accept the job, without printing.
func (c *Client) ValidateJob(ctx context.Context, printer *Printer, r JobRequest) error {
	m := c.newRequest(goipp.OpValidateJob)
	if r.Format != "" {
		m.Operation.Add(goipp.MakeAttr("document-format", goipp.TagMimeType, goipp.String(r.Format)))
	}
	jobTemplate(m, printer, r)
	_, err := c.do(ctx, "Validate-Job", m, nil)
	return err
}

var jobAttributes = []goipp.Value{
	goipp.String("job-id"), goipp.String("job-uri"), goipp.String("job-name"), goipp.String("job-state"),
	goipp.String("job-state-reasons"), goipp.String("job-originating-user-name"),
	goipp.String("job-impressions-completed"), goipp.String("date-time-at-creation"),
	goipp.String("date-time-at-completed"),
}

// Jobs lists jobs. which is "not-completed" (the queue) or "completed" (history).
func (c *Client) Jobs(ctx context.Context, which string, limit int) ([]Job, error) {
	m := c.newRequest(goipp.OpGetJobs)
	if which == "" {
		which = "not-completed"
	}
	m.Operation.Add(goipp.MakeAttr("which-jobs", goipp.TagKeyword, goipp.String(which)))
	if limit > 0 {
		m.Operation.Add(goipp.MakeAttr("limit", goipp.TagInteger, goipp.Integer(limit)))
	}
	m.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, jobAttributes[0], jobAttributes[1:]...))
	out, err := c.do(ctx, "Get-Jobs", m, nil)
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, g := range out.Groups {
		if g.Tag == goipp.TagJobGroup {
			jobs = append(jobs, *parseJob(index(g.Attrs)))
		}
	}
	return jobs, nil
}

// Job reads one job.
func (c *Client) Job(ctx context.Context, id int) (*Job, error) {
	m := c.newRequest(goipp.OpGetJobAttributes)
	m.Operation.Add(goipp.MakeAttr("job-id", goipp.TagInteger, goipp.Integer(id)))
	m.Operation.Add(goipp.MakeAttr("requested-attributes", goipp.TagKeyword, jobAttributes[0], jobAttributes[1:]...))
	out, err := c.do(ctx, "Get-Job-Attributes", m, nil)
	if err != nil {
		return nil, err
	}
	job := parseJob(index(out.Job))
	if job.ID == 0 {
		job.ID = id
	}
	return job, nil
}

// CancelJob cancels a job that hasn't finished.
func (c *Client) CancelJob(ctx context.Context, id int) error {
	m := c.newRequest(goipp.OpCancelJob)
	m.Operation.Add(goipp.MakeAttr("job-id", goipp.TagInteger, goipp.Integer(id)))
	_, err := c.do(ctx, "Cancel-Job", m, nil)
	return err
}

// Identify makes the printer flash or beep, so a person can tell which one it is.
func (c *Client) Identify(ctx context.Context) error {
	m := c.newRequest(goipp.OpIdentifyPrinter)
	_, err := c.do(ctx, "Identify-Printer", m, nil)
	return err
}

func parseJob(a attrs) *Job {
	j := &Job{
		URI:          a.str("job-uri"),
		Name:         a.str("job-name"),
		User:         a.str("job-originating-user-name"),
		StateReasons: nil,
	}
	j.ID, _ = a.integer("job-id")
	j.Impressions, _ = a.integer("job-impressions-completed")
	for _, r := range a.strs("job-state-reasons") {
		if r != "none" {
			j.StateReasons = append(j.StateReasons, r)
		}
	}
	switch n, _ := a.integer("job-state"); n {
	case 3:
		j.State = "pending"
	case 4:
		j.State = "held"
	case 5:
		j.State = "processing"
	case 6:
		j.State = "stopped"
	case 7:
		j.State = "canceled"
	case 8:
		j.State = "aborted"
	case 9:
		j.State = "completed"
	default:
		j.State = "unknown"
	}
	j.CreatedAt = a.time("date-time-at-creation")
	j.CompletedAt = a.time("date-time-at-completed")
	return j
}

// attrs indexes a group of attributes by name.
type attrs map[string]goipp.Values

func index(list goipp.Attributes) attrs {
	a := make(attrs, len(list))
	for _, at := range list {
		a[at.Name] = at.Values
	}
	return a
}

func (a attrs) str(name string) string {
	if v := a[name]; len(v) > 0 {
		return valueString(v[0].V)
	}
	return ""
}

func (a attrs) strs(name string) []string {
	var out []string
	for _, v := range a[name] {
		out = append(out, valueString(v.V))
	}
	return out
}

func (a attrs) integer(name string) (int, bool) {
	if v := a[name]; len(v) > 0 {
		if n, ok := v[0].V.(goipp.Integer); ok {
			return int(n), true
		}
	}
	return 0, false
}

func (a attrs) ints(name string) []int {
	var out []int
	for _, v := range a[name] {
		if n, ok := v.V.(goipp.Integer); ok {
			out = append(out, int(n))
		}
	}
	return out
}

func (a attrs) boolean(name string) bool {
	if v := a[name]; len(v) > 0 {
		if b, ok := v[0].V.(goipp.Boolean); ok {
			return bool(b)
		}
	}
	return false
}

func (a attrs) time(name string) time.Time {
	if v := a[name]; len(v) > 0 {
		if t, ok := v[0].V.(goipp.Time); ok {
			return t.Time
		}
	}
	return time.Time{}
}

func valueString(v goipp.Value) string {
	switch t := v.(type) {
	case goipp.String:
		return string(t)
	case goipp.TextWithLang:
		return t.Text
	case goipp.Binary:
		return string(t)
	default:
		return v.String()
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // don't cut a UTF-8 sequence in half
		n--
	}
	return s[:n]
}
