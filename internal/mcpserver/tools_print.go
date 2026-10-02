package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/store"
)

// ---- list_devices ------------------------------------------------------------

// printerOut is a printer as tools report it: what a model needs to decide
// whether and how to print, without the full IPP attribute dump.
type printerOut struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Default      bool     `json:"default"`
	Online       bool     `json:"online" jsonschema:"false when the printer could not be reached"`
	Error        string   `json:"error,omitempty"`
	Summary      string   `json:"summary" jsonschema:"one sentence describing the printer's state"`
	State        string   `json:"state,omitempty" jsonschema:"idle, processing or stopped"`
	StateReasons []string `json:"state_reasons,omitempty" jsonschema:"why the printer is not idle, e.g. media-empty, marker-supply-low"`
	MakeAndModel string   `json:"make_and_model,omitempty"`
	QueuedJobs   int      `json:"queued_jobs"`
	Ink          []inkOut `json:"ink,omitempty" jsonschema:"ink or toner supplies"`
	PaperLoaded  []string `json:"paper_loaded,omitempty" jsonschema:"paper sizes the printer reports as loaded"`
	CanDuplex    bool     `json:"can_duplex"`
	CanColor     bool     `json:"can_color"`
	// NativePDF is false for printers that only take raster data; Platen then
	// renders PDFs itself, which works but takes a little longer.
	NativePDF bool `json:"native_pdf"`
}

type inkOut struct {
	Name    string `json:"name"`
	Color   string `json:"color,omitempty" jsonschema:"colour as #RRGGBB when known"`
	Percent int    `json:"percent" jsonschema:"fill level in percent, or -1 when unknown"`
	Low     bool   `json:"low"`
}

func toPrinterOut(p hub.PrinterStatus) printerOut {
	out := printerOut{ID: p.ID, Name: p.Name, Default: p.Default, Online: p.Online, Error: p.Error, Summary: p.Summary}
	if p.Printer == nil {
		return out
	}
	out.State, out.StateReasons, out.MakeAndModel, out.QueuedJobs = p.State, p.StateReasons, p.MakeAndModel, p.QueuedJobs
	out.PaperLoaded = p.MediaLabels
	out.NativePDF = p.Supports("application/pdf")
	for _, sides := range p.Sides {
		out.CanDuplex = out.CanDuplex || sides != "one-sided"
	}
	for _, mode := range p.ColorModes {
		out.CanColor = out.CanColor || mode == "color"
	}
	for _, m := range p.Markers {
		out.Ink = append(out.Ink, inkOut{Name: m.Name, Color: m.Color, Percent: m.Level, Low: m.Level >= 0 && m.Level <= max(m.Low, 10)})
	}
	return out
}

type devicesOut struct {
	Printers  []printerOut        `json:"printers" jsonschema:"the configured printers with their current state"`
	Scanners  []hub.ScannerStatus `json:"scanners" jsonschema:"the configured scanners with their current state"`
	Paperless paperlessInfo       `json:"paperless" jsonschema:"whether documents can be filed in Paperless-ngx"`
	WebURL    string              `json:"web_url" jsonschema:"address of the Platen web interface for people"`
}

type paperlessInfo struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`
}

// ---- printer_status ----------------------------------------------------------

type printerIn struct {
	Printer string `json:"printer,omitempty" jsonschema:"printer id from list_devices; leave empty for the default printer"`
}

// ---- print_document ----------------------------------------------------------

type printIn struct {
	URL                 string `json:"url,omitempty" jsonschema:"address of a PDF, picture or text file for Platen to download and print"`
	PaperlessDocumentID int    `json:"paperless_document_id,omitempty" jsonschema:"id of a document in Paperless-ngx (see search_paperless)"`
	ScanID              string `json:"scan_id,omitempty" jsonschema:"id of a scan made with scan_document, to print a copy of it"`
	Text                string `json:"text,omitempty" jsonschema:"plain text to print as it is"`
	FilePath            string `json:"file_path,omitempty" jsonschema:"path of a file on the machine Platen runs on, inside an allowed directory"`
	ContentBase64       string `json:"content_base64,omitempty" jsonschema:"the document itself, base64 encoded (PDF, JPEG, PNG or text)"`
	Filename            string `json:"filename,omitempty" jsonschema:"file name to show for content_base64"`

	Printer string `json:"printer,omitempty" jsonschema:"printer id; leave empty for the default printer"`
	Title   string `json:"title,omitempty" jsonschema:"job title shown in the print queue"`
	Copies  int    `json:"copies,omitempty" jsonschema:"number of copies (default 1)"`
	Duplex  string `json:"duplex,omitempty" jsonschema:"off (default), long-edge (book style) or short-edge (flip up)"`
	Color   string `json:"color,omitempty" jsonschema:"auto (default), color or monochrome"`
	Quality string `json:"quality,omitempty" jsonschema:"draft, normal (default) or high"`
	Media   string `json:"media,omitempty" jsonschema:"paper size: a4, letter, legal, a5, 4x6, 5x7; leave empty for what the printer has loaded"`
	Pages   string `json:"pages,omitempty" jsonschema:"pages to print, for example 1-3,5; leave empty for all pages"`
	Fill    bool   `json:"fill,omitempty" jsonschema:"for pictures: cover the whole sheet, cropping what does not fit"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"only report how many pages and sheets the job takes; nothing is printed"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"set to true only after the person agreed to a job above the sheet threshold"`
}

// ---- jobs --------------------------------------------------------------------

type jobsIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many recent jobs to return (default 10)"`
}

type jobsOut struct {
	Jobs []store.PrintJob `json:"jobs" jsonschema:"print jobs sent through Platen, newest first"`
}

type cancelIn struct {
	JobID string `json:"job_id" jsonschema:"job id from print_document or list_print_jobs (starts with job_)"`
}

func (s *server) addPrintTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "list_devices",
		Title: "List printers and scanners",
		Description: "List the printers and scanners Platen knows, with their state, and say whether Paperless-ngx is connected. " +
			"Start here when you don't know what is available.",
		Annotations: readOnly("List printers and scanners"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, devicesOut, error) {
		out := devicesOut{Printers: []printerOut{}, Scanners: s.hub.Scanners(ctx), WebURL: s.baseURL}
		for _, p := range s.hub.Printers(ctx) {
			out.Printers = append(out.Printers, toPrinterOut(p))
		}
		if out.Scanners == nil {
			out.Scanners = []hub.ScannerStatus{}
		}
		if s.hub.PaperlessEnabled() {
			out.Paperless = paperlessInfo{Enabled: true, URL: s.hub.Config().Paperless.PublicURL}
		}
		msg := ""
		for _, p := range out.Printers {
			msg += fmt.Sprintf("Printer %q: %s\n", p.ID, p.Summary)
		}
		for _, sc := range out.Scanners {
			msg += fmt.Sprintf("Scanner %q: %s\n", sc.ID, sc.Summary)
		}
		if out.Paperless.Enabled {
			msg += "Paperless-ngx is connected."
		} else {
			msg += "Paperless-ngx is not connected."
		}
		return &mcp.CallToolResult{Content: text("%s", msg)}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "printer_status",
		Title: "Printer status",
		Description: "Read a printer's state: whether it is ready, why not if it isn't, ink or toner levels, loaded paper and queued jobs. " +
			"Call it before printing when you are unsure the printer is ready.",
		Annotations: readOnly("Printer status"),
		Meta:        uiMeta(uiPrinter),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in printerIn) (*mcp.CallToolResult, printerOut, error) {
		st, err := s.hub.Printer(ctx, in.Printer)
		if err != nil {
			return nil, printerOut{}, err
		}
		out := toPrinterOut(st)
		msg := out.Summary
		for _, ink := range out.Ink {
			if ink.Percent >= 0 {
				msg += fmt.Sprintf("\n%s: %d%%", ink.Name, ink.Percent)
			}
		}
		if len(out.PaperLoaded) > 0 {
			msg += fmt.Sprintf("\nPaper loaded: %s", out.PaperLoaded[0])
		}
		return &mcp.CallToolResult{Content: text("%s", msg)}, out, nil
	})

	printSchema, err := jsonschema.For[printIn](nil)
	if err != nil {
		panic(err)
	}
	printSchema.Properties["duplex"].Enum = []any{"off", "long-edge", "short-edge"}
	printSchema.Properties["color"].Enum = []any{"auto", "color", "monochrome"}
	printSchema.Properties["quality"].Enum = []any{"draft", "normal", "high"}
	printSchema.Properties["copies"].Minimum = jsonschema.Ptr(1.0)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "print_document",
		Title: "Print a document",
		Description: "Print a document on paper. Give exactly one source: url, paperless_document_id, scan_id, text, file_path or content_base64. " +
			"PDF, JPEG, PNG and plain text work on every printer; Platen converts them when the printer can't read them itself. " +
			"This uses real paper and ink, so print only what the person asked for. Use dry_run first for anything large. " +
			"Jobs above the sheet threshold need the person's agreement.",
		Annotations: additive("Print a document"),
		InputSchema: printSchema,
	}, s.printDocument)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_print_jobs",
		Title:       "Recent print jobs",
		Description: "List the print jobs sent through Platen, newest first, with their state (pending, processing, completed, canceled, aborted).",
		Annotations: readOnly("Recent print jobs"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in jobsIn) (*mcp.CallToolResult, jobsOut, error) {
		if in.Limit <= 0 {
			in.Limit = 10
		}
		out := jobsOut{Jobs: s.hub.History(min(in.Limit, 100))}
		if out.Jobs == nil {
			out.Jobs = []store.PrintJob{}
		}
		msg := fmt.Sprintf("%d job(s).", len(out.Jobs))
		for _, j := range out.Jobs {
			msg += fmt.Sprintf("\n%s  %-10s  %q, %d page(s), %d sheet(s), %s", j.ID, j.State, j.Title, j.Pages*j.Copies, j.Sheets, j.CreatedAt.Format("2006-01-02 15:04"))
		}
		return &mcp.CallToolResult{Content: text("%s", msg)}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cancel_print_job",
		Title:       "Cancel a print job",
		Description: "Cancel a print job that has not finished. Pages already printed stay printed.",
		Annotations: destructive("Cancel a print job"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cancelIn) (*mcp.CallToolResult, store.PrintJob, error) {
		job, err := s.hub.CancelJob(ctx, in.JobID)
		if err != nil {
			return nil, store.PrintJob{}, err
		}
		return &mcp.CallToolResult{Content: text("Canceled %q (%s).", job.Title, job.ID)}, job, nil
	})
}

func (s *server) printDocument(ctx context.Context, req *mcp.CallToolRequest, in printIn) (*mcp.CallToolResult, *hub.PrintResult, error) {
	pr := hub.PrintRequest{
		Printer: in.Printer, Title: in.Title, Copies: in.Copies, Duplex: in.Duplex, Color: in.Color, Quality: in.Quality,
		Media: in.Media, Pages: in.Pages, Fill: in.Fill, DryRun: in.DryRun, Via: "mcp",
		Source: hub.Source{
			URL: in.URL, PaperlessID: in.PaperlessDocumentID, ScanID: in.ScanID, Text: in.Text, Path: in.FilePath,
			Base64: in.ContentBase64, Name: in.Filename,
		},
	}
	if pr.Title == "" && in.ScanID != "" {
		pr.Title = "Copy of scan " + in.ScanID
	}
	const key = "confirm_print"
	canAsk := s.canAsk(req)

	// When the person can be asked directly, only their answer confirms a job.
	// Otherwise the model's confirm flag is accepted: it is told to ask first.
	if !canAsk {
		pr.Confirm = in.Confirm
	}
	if reply, ok := answer(req, key); ok {
		subject, err := s.verify(req.Params.RequestState, key)
		if err != nil {
			return nil, nil, err
		}
		if reply.Action != "accept" || reply.Content["confirm"] != true {
			return &mcp.CallToolResult{Content: text("The person declined. Nothing was printed.")}, &hub.PrintResult{State: "declined", Message: "The person declined. Nothing was printed."}, nil
		}
		// The confirmation counts only for the job it was given for.
		check := pr
		check.DryRun = true
		dry, err := s.hub.Print(ctx, check)
		if err != nil {
			return nil, nil, err
		}
		if subject != fingerprint(dry.Printer, dry.Title, dry.Pages, dry.Copies, dry.Sheets) {
			return nil, nil, errState
		}
		pr.Confirm = true
	}

	res, err := s.hub.Print(ctx, pr)
	var need *hub.ConfirmationRequired
	if errors.As(err, &need) {
		if !canAsk {
			return nil, nil, fmt.Errorf("%s. Ask the person whether to go ahead; if they agree, call print_document again with confirm set to true", need.Error())
		}
		dry := pr
		dry.DryRun = true
		check, derr := s.hub.Print(ctx, dry)
		if derr != nil {
			return nil, nil, derr
		}
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{key: &mcp.ElicitParams{
				Mode:    "form",
				Message: fmt.Sprintf("Print %q on %s? It takes %d sheet(s) of paper (%d page(s), %d cop%s).", need.Title, need.Printer, need.Sheets, need.Pages, need.Copies, map[bool]string{true: "y", false: "ies"}[need.Copies == 1]),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"confirm": {Type: "boolean", Title: "Print it", Description: "Use this much paper", Default: []byte("false")},
					},
					Required: []string{"confirm"},
				},
			}},
			RequestState: s.sign(key, fingerprint(check.Printer, check.Title, check.Pages, check.Copies, check.Sheets)),
		}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: text("%s", res.Message)}, res, nil
}
