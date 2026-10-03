// Package hub is Platen's core: it knows the configured printers and scanners,
// prepares and submits print jobs, runs scan sessions and files documents into
// Paperless-ngx. The web UI, the REST API and the MCP server are thin layers on
// top of it, so all three behave the same.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/escl"
	"github.com/giovannirco/platen/internal/ipp"
	"github.com/giovannirco/platen/internal/paperless"
	"github.com/giovannirco/platen/internal/render"
	"github.com/giovannirco/platen/internal/store"
)

// Errors that callers are expected to tell apart.
var (
	ErrNotFound      = errors.New("not found")
	ErrNoPrinter     = errors.New("no printer is configured")
	ErrNoScanner     = errors.New("no scanner is configured")
	ErrNoPaperless   = errors.New("Paperless-ngx is not configured")
	ErrScannerBusy   = errors.New("the scanner is busy with another scan")
	ErrPrinterBusy   = errors.New("the printer is busy")
	ErrInvalid       = errors.New("invalid request")
	ErrNotAllowed    = errors.New("not allowed")
	ErrUnsupported   = errors.New("unsupported")
	ErrLimitExceeded = errors.New("limit exceeded")
)

// invalid wraps a message as an ErrInvalid.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Hub ties devices, storage and Paperless together.
type Hub struct {
	cfg       *config.Config
	log       *slog.Logger
	store     *store.Store
	renderer  *render.Engine
	paperless *paperless.Client
	events    *broker

	printers map[string]*printerDev
	scanners map[string]*scannerDev

	scanMu sync.Mutex // guards changes to scan metadata

	fetcher *fetcher
}

// retryAfter is how long a device that did not answer is left alone. A printer
// that is switched off then costs one timeout, not one for every caller.
const retryAfter = 5 * time.Second

type printerDev struct {
	cfg    config.Printer
	client *ipp.Client

	mu       sync.Mutex
	attrs    *ipp.Printer
	fetched  time.Time
	lastErr  error
	failedAt time.Time
}

type scannerDev struct {
	cfg    config.Scanner
	client *escl.Client
	// busy is held while a scan job runs; scanning says so without blocking.
	busy     sync.Mutex
	scanning atomic.Bool

	mu       sync.Mutex
	caps     *escl.Capabilities
	fetched  time.Time
	status   *escl.Status
	statusAt time.Time
	lastErr  error
	failedAt time.Time
}

// New builds a Hub from the configuration.
func New(cfg *config.Config, log *slog.Logger) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	st, err := store.Open(cfg.Server.DataDir)
	if err != nil {
		return nil, err
	}
	h := &Hub{
		cfg: cfg, log: log, store: st,
		renderer: render.NewEngine(),
		events:   newBroker(),
		printers: map[string]*printerDev{},
		scanners: map[string]*scannerDev{},
		fetcher:  newFetcher(cfg.Fetch, int64(cfg.Limits.MaxUploadMB)<<20),
	}
	for _, p := range cfg.Printers {
		client, err := ipp.New(p.URI, ipp.Options{InsecureTLS: p.InsecureTLS})
		if err != nil {
			return nil, fmt.Errorf("printer %s: %w", p.ID, err)
		}
		h.printers[p.ID] = &printerDev{cfg: p, client: client}
	}
	for _, s := range cfg.Scanners {
		client, err := escl.New(s.URL, escl.Options{InsecureTLS: s.InsecureTLS})
		if err != nil {
			return nil, fmt.Errorf("scanner %s: %w", s.ID, err)
		}
		h.scanners[s.ID] = &scannerDev{cfg: s, client: client}
	}
	if cfg.Paperless.Enabled() {
		h.paperless = paperless.New(cfg.Paperless.URL, cfg.Paperless.Token, nil)
	}
	return h, nil
}

// ResumeJobs picks up print jobs that were still running when Platen last
// stopped, so the history doesn't show them as printing forever.
func (h *Hub) ResumeJobs() {
	for _, j := range h.store.Jobs(0) {
		switch j.State {
		case "pending", "held", "processing", "stopped":
		default:
			continue
		}
		dev, ok := h.printers[j.Printer]
		if !ok || time.Since(j.CreatedAt) > 2*time.Hour {
			h.updateJob(j.ID, "unknown", "Platen was not running when this job finished")
			continue
		}
		go h.watchJob(dev, j.ID, j.PrinterJob)
	}
}

// Close releases resources.
func (h *Hub) Close() error { return h.renderer.Close() }

// Config returns the configuration the hub runs with.
func (h *Hub) Config() *config.Config { return h.cfg }

// RunMaintenance removes old scans until ctx ends.
func (h *Hub) RunMaintenance(ctx context.Context) {
	if h.cfg.Limits.ScanRetentionDays <= 0 {
		return
	}
	tick := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	for {
		cutoff := time.Now().AddDate(0, 0, -h.cfg.Limits.ScanRetentionDays)
		if n := h.store.Prune(cutoff); n > 0 {
			h.log.Info("removed old scans", "count", n, "older_than_days", h.cfg.Limits.ScanRetentionDays)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ---- printers ----------------------------------------------------------------

// PrinterStatus is a printer as callers see it: configuration plus live state.
type PrinterStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Default bool   `json:"default"`
	// Online is false when the printer could not be reached; Error says why.
	Online bool   `json:"online"`
	Error  string `json:"error,omitempty"`
	// Summary is one sentence a person or a model can read.
	Summary string `json:"summary"`
	*ipp.Printer
	// MediaLabels gives short names for the paper sizes the printer reports as loaded.
	MediaLabels []string `json:"media_labels,omitempty"`
}

func (h *Hub) printer(id string) (*printerDev, error) {
	if len(h.printers) == 0 {
		return nil, ErrNoPrinter
	}
	if id == "" {
		if d := h.cfg.DefaultPrinter(); d != nil {
			return h.printers[d.ID], nil
		}
	}
	if p, ok := h.printers[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: printer %q (configured: %v)", ErrNotFound, id, h.printerIDs())
}

func (h *Hub) printerIDs() []string {
	ids := make([]string, 0, len(h.printers))
	for _, p := range h.cfg.Printers {
		ids = append(ids, p.ID)
	}
	return ids
}

// attributes returns the printer's attributes, at most maxAge old.
func (p *printerDev) attributes(ctx context.Context, maxAge time.Duration) (*ipp.Printer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attrs != nil && time.Since(p.fetched) < maxAge {
		return p.attrs, nil
	}
	if p.lastErr != nil && time.Since(p.failedAt) < retryAfter {
		return p.known()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	attrs, err := p.client.PrinterAttributes(fetchCtx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // the caller gave up; that says nothing about the printer
		}
		p.lastErr, p.failedAt = err, time.Now()
		return p.known()
	}
	p.attrs, p.fetched, p.lastErr = attrs, time.Now(), nil
	return attrs, nil
}

// known answers after a failed fetch: what the printer said less than a minute
// ago (a short hiccup shouldn't show as offline), or else the error.
func (p *printerDev) known() (*ipp.Printer, error) {
	if p.attrs != nil && time.Since(p.fetched) < time.Minute {
		return p.attrs, nil
	}
	return nil, p.lastErr
}

func (h *Hub) printerStatus(ctx context.Context, p *printerDev, maxAge time.Duration) PrinterStatus {
	st := PrinterStatus{ID: p.cfg.ID, Name: p.cfg.Name, Default: p.cfg.Default}
	attrs, err := p.attributes(ctx, maxAge)
	if err != nil {
		st.Error = err.Error()
		st.Summary = fmt.Sprintf("%s is not reachable: %v", p.cfg.Name, err)
		return st
	}
	st.Online, st.Printer = true, attrs
	// A printer lists the paper in its trays; a print server lists every size it
	// knows, which says nothing. In that case the default size is the best guess.
	loaded := attrs.MediaReady
	if len(loaded) == 0 || len(loaded) > 2 {
		loaded = nil
		if attrs.MediaDefault != "" {
			loaded = []string{attrs.MediaDefault}
		}
	}
	for _, m := range loaded {
		if media, ok := ipp.ParseMedia(m); ok {
			st.MediaLabels = append(st.MediaLabels, media.Label())
		}
	}
	st.Summary = summarize(p.cfg.Name, attrs)
	return st
}

func summarize(name string, a *ipp.Printer) string {
	s := fmt.Sprintf("%s is %s", name, a.State)
	if len(a.StateReasons) > 0 {
		s += fmt.Sprintf(" (%s)", joinMax(a.StateReasons, 3))
	}
	if a.QueuedJobs > 0 {
		s += fmt.Sprintf(", %d job(s) queued", a.QueuedJobs)
	}
	var low []string
	for _, m := range a.Markers {
		if m.Level >= 0 && m.Level <= max(m.Low, 10) {
			low = append(low, fmt.Sprintf("%s %d%%", m.Name, m.Level))
		}
	}
	if len(low) > 0 {
		s += "; low: " + joinMax(low, 4)
	}
	return s + "."
}

// Printers returns every configured printer with its state.
func (h *Hub) Printers(ctx context.Context) []PrinterStatus {
	out := make([]PrinterStatus, len(h.cfg.Printers))
	var wg sync.WaitGroup
	for i, c := range h.cfg.Printers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = h.printerStatus(ctx, h.printers[c.ID], 5*time.Second)
		}()
	}
	wg.Wait()
	return out
}

// Printer returns one printer with fresh state. An empty id means the default.
func (h *Hub) Printer(ctx context.Context, id string) (PrinterStatus, error) {
	p, err := h.printer(id)
	if err != nil {
		return PrinterStatus{}, err
	}
	return h.printerStatus(ctx, p, 2*time.Second), nil
}

// IdentifyPrinter makes the printer flash or beep.
func (h *Hub) IdentifyPrinter(ctx context.Context, id string) error {
	p, err := h.printer(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return p.client.Identify(ctx)
}

// QueueJobs lists the jobs the printer itself knows about.
func (h *Hub) QueueJobs(ctx context.Context, id, which string, limit int) ([]ipp.Job, error) {
	p, err := h.printer(id)
	if err != nil {
		return nil, err
	}
	if which != "completed" {
		which = "not-completed"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	jobs, err := p.client.Jobs(ctx, which, limit)
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID > jobs[j].ID })
	return jobs, nil
}

// History returns Platen's own record of print jobs, newest first.
func (h *Hub) History(limit int) []store.PrintJob { return h.store.Jobs(limit) }

// ---- scanners ----------------------------------------------------------------

// ScannerStatus is a scanner as callers see it.
type ScannerStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Default bool   `json:"default"`
	Online  bool   `json:"online"`
	Error   string `json:"error,omitempty"`
	// State is Idle, Processing, ... as the scanner reports it.
	State        string `json:"state,omitempty"`
	FeederState  string `json:"feeder_state,omitempty"`
	MakeAndModel string `json:"make_and_model,omitempty"`
	// Sources lists what can be scanned from: flatbed, feeder.
	Sources     []string `json:"sources,omitempty"`
	Resolutions []int    `json:"resolutions,omitempty"`
	ColorModes  []string `json:"color_modes,omitempty"` // color, gray
	// MaxWidthMM and MaxHeightMM give the size of the flatbed (or feeder) area.
	MaxWidthMM  float64 `json:"max_width_mm,omitempty"`
	MaxHeightMM float64 `json:"max_height_mm,omitempty"`
	Summary     string  `json:"summary"`
}

func (h *Hub) scanner(id string) (*scannerDev, error) {
	if len(h.scanners) == 0 {
		return nil, ErrNoScanner
	}
	if id == "" {
		if d := h.cfg.DefaultScanner(); d != nil {
			return h.scanners[d.ID], nil
		}
	}
	if s, ok := h.scanners[id]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("%w: scanner %q", ErrNotFound, id)
}

func (s *scannerDev) capabilities(ctx context.Context) (*escl.Capabilities, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.caps != nil && time.Since(s.fetched) < 10*time.Minute {
		return s.caps, nil
	}
	if s.lastErr != nil && time.Since(s.failedAt) < retryAfter {
		return nil, s.lastErr
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	caps, err := s.client.Capabilities(fetchCtx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.lastErr, s.failedAt = err, time.Now()
		return nil, err
	}
	s.caps, s.fetched, s.lastErr = caps, time.Now(), nil
	return caps, nil
}

// state reads what the scanner is doing. While Platen itself runs a scan the
// answer is known, and the scanner is not asked.
func (s *scannerDev) state(ctx context.Context) (*escl.Status, error) {
	if s.scanning.Load() {
		return &escl.Status{State: "Processing"}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != nil && time.Since(s.statusAt) < time.Second {
		return s.status, nil
	}
	if s.lastErr != nil && time.Since(s.failedAt) < retryAfter {
		return nil, s.lastErr
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := s.client.Status(fetchCtx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.status, s.lastErr, s.failedAt = nil, err, time.Now()
		return nil, err
	}
	s.status, s.statusAt, s.lastErr = status, time.Now(), nil
	return status, nil
}

func (h *Hub) scannerStatus(ctx context.Context, s *scannerDev) ScannerStatus {
	st := ScannerStatus{ID: s.cfg.ID, Name: s.cfg.Name, Default: s.cfg.Default}
	caps, err := s.capabilities(ctx)
	if err != nil {
		st.Error = err.Error()
		st.Summary = fmt.Sprintf("%s is not reachable: %v", s.cfg.Name, err)
		return st
	}
	st.Online, st.MakeAndModel = true, caps.MakeAndModel
	in := caps.Platen
	if in != nil {
		st.Sources = append(st.Sources, "flatbed")
	}
	if caps.Feeder != nil {
		st.Sources = append(st.Sources, "feeder")
		if in == nil {
			in = caps.Feeder
		}
	}
	if in != nil {
		st.Resolutions = append(st.Resolutions, in.Resolutions...)
		sort.Ints(st.Resolutions)
		for _, m := range in.ColorModes {
			switch m {
			case "RGB24":
				st.ColorModes = append(st.ColorModes, "color")
			case "Grayscale8":
				st.ColorModes = append(st.ColorModes, "gray")
			}
		}
		st.MaxWidthMM = float64(in.MaxWidth) * 25.4 / 300
		st.MaxHeightMM = float64(in.MaxHeight) * 25.4 / 300
	}
	// The capabilities are kept for minutes; the state shows whether the scanner
	// answers right now.
	status, err := s.state(ctx)
	if err != nil {
		st.Online, st.Error = false, err.Error()
		st.Summary = fmt.Sprintf("%s is not reachable: %v", s.cfg.Name, err)
		return st
	}
	st.State, st.FeederState = status.State, status.FeederState
	st.Summary = fmt.Sprintf("%s is %s.", s.cfg.Name, orDefault(strings.ToLower(st.State), "online"))
	return st
}

// Scanners returns every configured scanner with its state.
func (h *Hub) Scanners(ctx context.Context) []ScannerStatus {
	out := make([]ScannerStatus, len(h.cfg.Scanners))
	var wg sync.WaitGroup
	for i, c := range h.cfg.Scanners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = h.scannerStatus(ctx, h.scanners[c.ID])
		}()
	}
	wg.Wait()
	return out
}

// Scanner returns one scanner. An empty id means the default.
func (h *Hub) Scanner(ctx context.Context, id string) (ScannerStatus, error) {
	s, err := h.scanner(id)
	if err != nil {
		return ScannerStatus{}, err
	}
	return h.scannerStatus(ctx, s), nil
}

func joinMax(items []string, n int) string {
	out := ""
	for i, it := range items {
		if i == n {
			return out + fmt.Sprintf(" and %d more", len(items)-n)
		}
		if i > 0 {
			out += ", "
		}
		out += it
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
