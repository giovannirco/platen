package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/mcpserver"
	"github.com/giovannirco/platen/internal/pdfw"
	"github.com/giovannirco/platen/internal/testutil"
)

type fixture struct {
	hub       *hub.Hub
	printer   *testutil.Printer
	scanner   *testutil.Scanner
	paperless *testutil.Paperless
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{printer: testutil.NewRasterPrinter(t), scanner: testutil.NewScanner(t), paperless: testutil.NewPaperless(t)}
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.BaseURL = "http://platen.test:8080"
	cfg.Limits.ConfirmAboveSheets = 1
	cfg.Printers = []config.Printer{{ID: "inkjet", Name: "Inkjet", URI: f.printer.URI()}}
	cfg.Scanners = []config.Scanner{{ID: "flatbed", Name: "Flatbed", URL: f.scanner.BaseURL()}}
	cfg.Paperless = config.Paperless{URL: f.paperless.URL, Token: testutil.PaperlessToken}
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

// asker plays the person at the keyboard: it answers the questions Platen asks.
type asker struct {
	mu       sync.Mutex
	answers  []map[string]any // one per question, in order; nil declines
	messages []string
}

func (a *asker) handle(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, req.Params.Message)
	if len(a.answers) == 0 {
		return &mcp.ElicitResult{Action: "cancel"}, nil
	}
	answer := a.answers[0]
	a.answers = a.answers[1:]
	if answer == nil {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	return &mcp.ElicitResult{Action: "accept", Content: answer}, nil
}

// connect links a client to the server over an in-memory transport (as stdio would).
func connect(t *testing.T, f *fixture, a *asker) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcpserver.New(f.hub, mcpserver.Options{Version: "test"})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	return dial(t, ct, a)
}

// connectHTTP links a client to the stateless HTTP endpoint, as a remote agent would.
func connectHTTP(t *testing.T, f *fixture, a *asker) *mcp.ClientSession {
	t.Helper()
	srv := mcpserver.New(f.hub, mcpserver.Options{Version: "test", Stateless: true})
	ts := httptest.NewServer(mcpserver.HTTPHandler(srv, nil))
	t.Cleanup(ts.Close)
	return dial(t, &mcp.StreamableClientTransport{Endpoint: ts.URL}, a)
}

func dial(t *testing.T, transport mcp.Transport, a *asker) *mcp.ClientSession {
	t.Helper()
	opts := &mcp.ClientOptions{}
	if a != nil {
		opts.ElicitationHandler = a.handle
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, opts)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(raw, &out)
	}
	return res, out
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func threePagePDF(t *testing.T) []byte {
	t.Helper()
	var page bytes.Buffer
	if err := jpeg.Encode(&page, image.NewGray(image.Rect(0, 0, 310, 438)), nil); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	pages := []pdfw.Page{{JPEG: page.Bytes(), DPI: 37}, {JPEG: page.Bytes(), DPI: 37}, {JPEG: page.Bytes(), DPI: 37}}
	if err := pdfw.Write(&out, pages, pdfw.Info{}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestToolsAreDescribed(t *testing.T) {
	cs := connect(t, newFixture(t), nil)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	var order []string
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
		order = append(order, tool.Name)
	}
	want := []string{"cancel_print_job", "delete_scan", "file_scan_in_paperless", "list_devices", "list_print_jobs", "list_scans", "print_document", "printer_status", "scan_document", "search_paperless"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("tools (in the order sent): %v", order)
	}
	for name, tool := range tools {
		if tool.Description == "" || tool.Title == "" || tool.Annotations == nil || tool.OutputSchema == nil {
			t.Errorf("%s lacks a description, title, annotations or output schema", name)
		}
	}
	if a := tools["printer_status"].Annotations; !a.ReadOnlyHint {
		t.Error("printer_status should be read-only")
	}
	if a := tools["print_document"].Annotations; a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.IdempotentHint {
		t.Errorf("print_document annotations: %+v", a)
	}
	if a := tools["delete_scan"].Annotations; a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Error("delete_scan should be marked destructive")
	}
	for name, uri := range map[string]string{"printer_status": "ui://platen/printer-status.html", "scan_document": "ui://platen/scan.html"} {
		ui, _ := tools[name].Meta["ui"].(map[string]any)
		if ui["resourceUri"] != uri {
			t.Errorf("%s: _meta.ui.resourceUri = %v", name, ui["resourceUri"])
		}
	}
	raw, _ := json.Marshal(tools["print_document"].InputSchema)
	if !strings.Contains(string(raw), `"long-edge"`) || !strings.Contains(string(raw), `"dry_run"`) {
		t.Errorf("print_document input schema lacks enums or fields: %s", raw)
	}
}

func TestDevicesAndStatus(t *testing.T) {
	cs := connect(t, newFixture(t), nil)
	res, out := call(t, cs, "list_devices", nil)
	if res.IsError || !strings.Contains(textOf(res), `Printer "inkjet"`) {
		t.Fatalf("list_devices: %s", textOf(res))
	}
	printers := out["printers"].([]any)
	p := printers[0].(map[string]any)
	if p["online"] != true || p["native_pdf"] != false || p["can_duplex"] != true {
		t.Errorf("printer: %v", p)
	}
	if pl := out["paperless"].(map[string]any); pl["enabled"] != true {
		t.Errorf("paperless: %v", pl)
	}
	res, out = call(t, cs, "printer_status", map[string]any{"printer": "inkjet"})
	ink := out["ink"].([]any)
	magenta := ink[2].(map[string]any)
	if len(ink) != 4 || magenta["percent"] != float64(8) || magenta["low"] != true || !strings.Contains(textOf(res), "Magenta: 8%") {
		t.Errorf("ink: %v / %s", ink, textOf(res))
	}
	res, _ = call(t, cs, "printer_status", map[string]any{"printer": "nope"})
	if !res.IsError {
		t.Error("an unknown printer should be a tool error")
	}
}

func TestPrintAsksThePerson(t *testing.T) {
	for _, mode := range []string{"stdio", "http"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			a := &asker{answers: []map[string]any{{"confirm": true}}}
			cs := connect(t, f, a)
			if mode == "http" {
				cs = connectHTTP(t, f, a)
			}
			args := map[string]any{"text": strings.Repeat("line\n", 400), "title": "Long list"}

			// A dry run never asks and never prints.
			res, out := call(t, cs, "print_document", map[string]any{"text": args["text"], "dry_run": true})
			if res.IsError || out["state"] != "not-printed" || out["sheets"].(float64) < 2 || len(a.messages) != 0 {
				t.Fatalf("dry run: %v / asked %d", out, len(a.messages))
			}

			// The model claiming confirmation is not enough when the person can be asked.
			args["confirm"] = true
			res, out = call(t, cs, "print_document", args)
			if res.IsError {
				t.Fatalf("print: %s", textOf(res))
			}
			if len(a.messages) != 1 || !strings.Contains(a.messages[0], "Long list") || !strings.Contains(a.messages[0], "sheet") {
				t.Fatalf("the person was asked %d time(s): %v", len(a.messages), a.messages)
			}
			if out["printer_job_id"] != float64(1) || len(f.printer.Jobs()) != 1 {
				t.Fatalf("job not printed after confirmation: %v", out)
			}

			// Declining prints nothing.
			a.answers = []map[string]any{nil}
			res, out = call(t, cs, "print_document", args)
			if res.IsError || out["state"] != "declined" || len(f.printer.Jobs()) != 1 {
				t.Fatalf("declined job: %v, %d job(s) on the printer", out, len(f.printer.Jobs()))
			}

			// One sheet is under the threshold: no question.
			a.messages = nil
			res, _ = call(t, cs, "print_document", map[string]any{"text": "short"})
			if res.IsError || len(a.messages) != 0 || len(f.printer.Jobs()) != 2 {
				t.Fatalf("small job: %s", textOf(res))
			}
		})
	}
}

func TestPrintWithoutAWayToAsk(t *testing.T) {
	f := newFixture(t)
	cs := connect(t, f, nil) // a client that can't show questions
	args := map[string]any{"content_base64": threePagePDF(t), "filename": "three.pdf"}
	res, _ := call(t, cs, "print_document", args)
	if !res.IsError || !strings.Contains(textOf(res), "confirm set to true") || len(f.printer.Jobs()) != 0 {
		t.Fatalf("expected an instruction to ask the person, got: %s", textOf(res))
	}
	args["confirm"] = true
	res, out := call(t, cs, "print_document", args)
	if res.IsError || out["pages"] != float64(3) || out["format"] != "image/pwg-raster" || len(f.printer.Jobs()) != 1 {
		t.Fatalf("confirmed job: %s / %v", textOf(res), out)
	}
	res, out = call(t, cs, "list_print_jobs", nil)
	jobs := out["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["title"] != "three.pdf" {
		t.Errorf("history: %v", jobs)
	}
	id := jobs[0].(map[string]any)["id"].(string)
	res, out = call(t, cs, "cancel_print_job", map[string]any{"job_id": id})
	if res.IsError || out["state"] != "canceled" || !f.printer.Jobs()[0].Canceled {
		t.Errorf("cancel: %s", textOf(res))
	}
}

func TestScanShowsThePageAndCanAskForMore(t *testing.T) {
	for _, mode := range []string{"stdio", "http"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			// Yes to a second page, no to a third.
			a := &asker{answers: []map[string]any{{"another": true}, {"another": false}}}
			cs := connect(t, f, a)
			if mode == "http" {
				cs = connectHTTP(t, f, a)
			}
			res, out := call(t, cs, "scan_document", map[string]any{"resolution": 150, "ask_for_more_pages": true, "title": "Letter"})
			if res.IsError {
				t.Fatalf("scan: %s", textOf(res))
			}
			if len(a.messages) != 2 || !strings.Contains(a.messages[0], "Page 1") || !strings.Contains(a.messages[1], "Page 2") {
				t.Fatalf("questions asked: %v", a.messages)
			}
			if out["page_count"] != float64(2) || len(f.scanner.Requests) != 2 {
				t.Fatalf("expected two pages, got %v (%d scans)", out["page_count"], len(f.scanner.Requests))
			}
			images, links := 0, 0
			for _, c := range res.Content {
				switch v := c.(type) {
				case *mcp.ImageContent:
					images++
					if cfg, err := jpeg.DecodeConfig(bytes.NewReader(v.Data)); err != nil || cfg.Height != 1200 {
						t.Errorf("page picture: %v %+v", err, cfg)
					}
				case *mcp.ResourceLink:
					links++
					if !strings.HasPrefix(v.URI, "platen://scans/") || v.MIMEType != "application/pdf" {
						t.Errorf("resource link: %+v", v)
					}
				}
			}
			if images != 2 || links != 1 {
				t.Errorf("content: %d picture(s), %d link(s)", images, links)
			}
			if p, _ := out["preview"].(string); !strings.HasPrefix(p, "data:image/jpeg;base64,") {
				t.Error("no preview for the in-chat view")
			}
			id := out["scan_id"].(string)
			if out["document_url"] != "http://platen.test:8080/api/v1/scans/"+id+"/document" {
				t.Errorf("document_url: %v", out["document_url"])
			}

			// One more page into the same scan, the way a model continues without the dialog.
			res, out = call(t, cs, "scan_document", map[string]any{"scan_id": id})
			if res.IsError || out["page_count"] != float64(3) {
				t.Fatalf("append: %s", textOf(res))
			}

			// The PDF is readable as a resource.
			rr, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "platen://scans/" + id + "/document"})
			if err != nil || len(rr.Contents) != 1 || !bytes.HasPrefix(rr.Contents[0].Blob, []byte("%PDF-")) {
				t.Fatalf("read scan document: %v", err)
			}
			rr, err = cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "platen://scans/" + id + "/pages/3"})
			if err != nil || rr.Contents[0].MIMEType != "image/jpeg" {
				t.Fatalf("read scan page: %v", err)
			}

			res, out = call(t, cs, "file_scan_in_paperless", map[string]any{"scan_id": id, "title": "Letter from the bank", "tags": []string{"bank"}, "created": "2026-09-30"})
			if res.IsError {
				t.Fatalf("file: %s", textOf(res))
			}
			filed := out["paperless"].(map[string]any)
			if filed["status"] != "success" || !strings.Contains(textOf(res), "/documents/") || len(f.paperless.Docs()) != 1 {
				t.Errorf("filing: %v", filed)
			}
			res, out = call(t, cs, "search_paperless", map[string]any{"query": "bank"})
			if docs := out["documents"].([]any); res.IsError || len(docs) != 1 || docs[0].(map[string]any)["title"] != "Letter from the bank" {
				t.Errorf("search: %v", out)
			}
			res, _ = call(t, cs, "list_scans", nil)
			if !strings.Contains(textOf(res), id) || !strings.Contains(textOf(res), "Paperless: success") {
				t.Errorf("list_scans: %s", textOf(res))
			}
			if res, _ = call(t, cs, "delete_scan", map[string]any{"scan_id": id}); res.IsError {
				t.Errorf("delete: %s", textOf(res))
			}
		})
	}
}

func TestViewsPromptsAndResources(t *testing.T) {
	cs := connect(t, newFixture(t), nil)
	ctx := context.Background()
	for _, uri := range []string{"ui://platen/printer-status.html", "ui://platen/scan.html"} {
		rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		c := rr.Contents[0]
		if c.MIMEType != "text/html;profile=mcp-app" || !strings.Contains(c.Text, "ui/initialize") || !strings.Contains(c.Text, "<!doctype html>") {
			t.Errorf("%s: mime %q, %d bytes", uri, c.MIMEType, len(c.Text))
		}
		// The views must work under the default MCP Apps content security policy:
		// nothing may be loaded from the network.
		for _, forbidden := range []string{"<script src", "<link ", "https://", "http://"} {
			if strings.Contains(c.Text, forbidden) {
				t.Errorf("%s references the network (%q)", uri, forbidden)
			}
		}
	}
	rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "platen://printers/inkjet"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, `"make_and_model": "Platen Test Inkjet"`) {
		t.Errorf("printer resource: %v", err)
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "platen://scans/scn_aaaaaaaaaa/document"}); err == nil {
		t.Error("a missing scan should not be readable")
	}
	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 2 {
		t.Fatalf("prompts: %v", err)
	}
	p, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "scan_and_file", Arguments: map[string]string{"hint": "water bill"}})
	if err != nil || !strings.Contains(p.Messages[0].Content.(*mcp.TextContent).Text, "water bill") {
		t.Errorf("prompt: %v", err)
	}
}
