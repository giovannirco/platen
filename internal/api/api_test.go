package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/giovannirco/platen/internal/api"
	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/testutil"
)

const token = "a-long-enough-test-token"

type fixture struct {
	srv     *httptest.Server
	printer *testutil.Printer
	scanner *testutil.Scanner
	token   string
}

func newFixture(t *testing.T, withAuth bool) *fixture {
	t.Helper()
	f := &fixture{printer: testutil.NewRasterPrinter(t), scanner: testutil.NewScanner(t)}
	pl := testutil.NewPaperless(t)
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Limits.ConfirmAboveSheets = 1
	cfg.Printers = []config.Printer{{ID: "inkjet", Name: "Inkjet", URI: f.printer.URI()}}
	cfg.Scanners = []config.Scanner{{ID: "flatbed", Name: "Flatbed", URL: f.scanner.BaseURL()}}
	cfg.Paperless = config.Paperless{URL: pl.URL, Token: testutil.PaperlessToken}
	if withAuth {
		cfg.Auth.Tokens, f.token = []string{token}, token
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.srv = httptest.NewServer(api.New(h, log, "test", http.NotFoundHandler()).Handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) do(t *testing.T, method, path string, body any, headers ...string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case *bytes.Buffer:
		r = b
	default:
		raw, _ := json.Marshal(b)
		r, contentType = bytes.NewReader(raw), "application/json"
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, r)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
		if headers[i] == "Host" {
			req.Host = headers[i+1]
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func errorCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestInfoAndDevices(t *testing.T) {
	f := newFixture(t, false)
	status, out := f.do(t, "GET", "/api/v1/info", nil)
	if status != 200 || out["name"] != "Platen" || out["paperless"] != true || out["auth"] != false {
		t.Fatalf("info: %d %v", status, out)
	}
	// Device addresses are reduced to scheme and host.
	if uri := out["printers"].([]any)[0].(map[string]any)["uri"].(string); strings.Contains(uri, "/ipp/print") {
		t.Errorf("printer uri leaked its path: %s", uri)
	}
	status, out = f.do(t, "GET", "/api/v1/printers", nil)
	if p := out["printers"].([]any)[0].(map[string]any); status != 200 || p["state"] != "idle" || len(p["markers"].([]any)) != 4 {
		t.Errorf("printers: %d %v", status, out)
	}
	if status, _ := f.do(t, "GET", "/api/v1/scanners/flatbed", nil); status != 200 {
		t.Errorf("scanner: %d", status)
	}
	if status, out := f.do(t, "GET", "/api/v1/printers/nope", nil); status != 404 || errorCode(out) != "not_found" {
		t.Errorf("unknown printer: %d %v", status, out)
	}
	if status, out := f.do(t, "GET", "/api/v1/nothing-here", nil); status != 404 || errorCode(out) != "not_found" {
		t.Errorf("unknown endpoint: %d %v", status, out)
	}
	resp, err := http.Get(f.srv.URL + "/api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil || spec["openapi"] != "3.1.0" || len(spec["paths"].(map[string]any)) < 15 {
		t.Errorf("openapi document: %v", err)
	}
}

func TestPrintUploadAndConfirmation(t *testing.T) {
	f := newFixture(t, false)
	form := func(fields map[string]string) (*bytes.Buffer, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", "notes.txt")
		_, _ = part.Write([]byte(strings.Repeat("a line of notes\n", 300)))
		for k, v := range fields {
			_ = mw.WriteField(k, v)
		}
		mw.Close()
		return &buf, mw.FormDataContentType()
	}
	body, ct := form(map[string]string{"dry_run": "true", "copies": "2"})
	status, out := f.do(t, "POST", "/api/v1/print", body, "Content-Type", ct)
	if status != 200 || out["dry_run"] != true || out["title"] != "notes.txt" || out["copies"] != float64(2) {
		t.Fatalf("dry run: %d %v", status, out)
	}
	body, ct = form(nil)
	status, out = f.do(t, "POST", "/api/v1/print", body, "Content-Type", ct)
	details, _ := out["error"].(map[string]any)["details"].(map[string]any)
	if status != 409 || errorCode(out) != "confirmation_required" || details["sheets"].(float64) < 2 {
		t.Fatalf("expected 409 confirmation_required, got %d %v", status, out)
	}
	if len(f.printer.Jobs()) != 0 {
		t.Fatal("printed without confirmation")
	}
	body, ct = form(map[string]string{"confirm": "true", "color": "monochrome"})
	status, out = f.do(t, "POST", "/api/v1/print", body, "Content-Type", ct, "X-Platen-Client", "web")
	if status != 200 || out["printer_job_id"] != float64(1) || len(f.printer.Jobs()) != 1 {
		t.Fatalf("confirmed print: %d %v", status, out)
	}
	_, out = f.do(t, "GET", "/api/v1/jobs", nil)
	job := out["jobs"].([]any)[0].(map[string]any)
	if job["via"] != "web" || job["title"] != "notes.txt" {
		t.Errorf("history entry: %v", job)
	}
	status, out = f.do(t, "POST", "/api/v1/print", map[string]any{"source": map[string]any{"text": "hi"}, "copies": 99})
	if status != 422 || errorCode(out) != "limit_exceeded" {
		t.Errorf("copies limit: %d %v", status, out)
	}
	status, out = f.do(t, "POST", "/api/v1/print", map[string]any{"unknown_field": 1})
	if status != 400 || errorCode(out) != "invalid" {
		t.Errorf("unknown field: %d %v", status, out)
	}
}

func TestScanFlow(t *testing.T) {
	f := newFixture(t, false)
	status, sc := f.do(t, "POST", "/api/v1/scans", map[string]any{"resolution": 75, "title": "Receipt"})
	if status != 201 || len(sc["pages"].([]any)) != 1 {
		t.Fatalf("start scan: %d %v", status, sc)
	}
	id := sc["id"].(string)
	if status, sc = f.do(t, "POST", "/api/v1/scans/"+id+"/pages", nil); status != 201 || len(sc["pages"].([]any)) != 2 {
		t.Fatalf("second page: %d", status)
	}
	page := sc["pages"].([]any)[0].(map[string]any)["id"].(string)
	resp, err := http.Get(f.srv.URL + "/api/v1/scans/" + id + "/pages/" + page + "/image?size=200")
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("page image: %v", err)
	}
	resp.Body.Close()
	if status, sc = f.do(t, "DELETE", "/api/v1/scans/"+id+"/pages/"+page, nil); status != 200 || len(sc["pages"].([]any)) != 1 {
		t.Fatalf("remove page: %d", status)
	}
	resp, err = http.Get(f.srv.URL + "/api/v1/scans/" + id + "/document")
	if err != nil {
		t.Fatal(err)
	}
	pdf, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) || !strings.Contains(resp.Header.Get("Content-Disposition"), "Receipt.pdf") {
		t.Errorf("document download: %q, %d bytes", resp.Header.Get("Content-Disposition"), len(pdf))
	}
	status, sc = f.do(t, "POST", "/api/v1/scans/"+id+"/paperless", map[string]any{"tags": []string{"receipts"}, "wait": true})
	if filed, _ := sc["paperless"].(map[string]any); status != 200 || filed["status"] != "success" {
		t.Fatalf("file in paperless: %d %v", status, sc)
	}
	// A scan can be printed straight away: a photocopy.
	status, out := f.do(t, "POST", "/api/v1/scans/"+id+"/print", map[string]any{})
	if status != 200 || out["pages"] != float64(1) {
		t.Errorf("copy: %d %v", status, out)
	}
	if status, _ := f.do(t, "GET", "/api/v1/paperless", nil); status != 200 {
		t.Errorf("paperless meta: %d", status)
	}
	if status, _ := f.do(t, "DELETE", "/api/v1/scans/"+id, nil); status != 200 {
		t.Errorf("delete: %d", status)
	}
	if status, _ := f.do(t, "GET", "/api/v1/scans/"+id, nil); status != 404 {
		t.Errorf("deleted scan: %d", status)
	}
}

func TestAccessControl(t *testing.T) {
	f := newFixture(t, true)
	good := f.token
	f.token = ""
	if status, out := f.do(t, "GET", "/api/v1/printers", nil); status != 401 || errorCode(out) != "unauthorized" {
		t.Errorf("no token: %d", status)
	}
	f.token = "wrong-token-wrong-token"
	if status, _ := f.do(t, "GET", "/api/v1/printers", nil); status != 401 {
		t.Errorf("wrong token: %d", status)
	}
	f.token = good
	if status, _ := f.do(t, "GET", "/api/v1/printers", nil); status != 200 {
		t.Errorf("good token: %d", status)
	}
	// The health check and the interface itself need no token.
	for _, path := range []string{"/healthz", "/", "/app.js"} {
		resp, err := http.Get(f.srv.URL + path)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("%s: %v", path, err)
		}
		resp.Body.Close()
	}
	// The browser logs in with the token and gets a cookie.
	resp, err := http.Post(f.srv.URL+"/api/v1/login", "application/json", strings.NewReader(`{"token":"`+good+`"}`))
	if err != nil || resp.StatusCode != 200 || len(resp.Cookies()) != 1 || !resp.Cookies()[0].HttpOnly {
		t.Fatalf("login: %v", err)
	}
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/v1/scanners", nil)
	req.AddCookie(resp.Cookies()[0])
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("cookie session: %d", resp.StatusCode)
	}
}

func TestBrowserAttacksAreRefused(t *testing.T) {
	f := newFixture(t, false)
	// A page on another site that makes the browser post to Platen.
	status, _ := f.do(t, "POST", "/api/v1/print", map[string]any{"source": map[string]any{"text": "spam"}},
		"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site")
	if status != 403 || len(f.printer.Jobs()) != 0 {
		t.Errorf("cross-site print: %d, %d job(s)", status, len(f.printer.Jobs()))
	}
	// DNS rebinding: the attacker's host name resolves to Platen's address.
	status, out := f.do(t, "GET", "/api/v1/printers", nil, "Host", "evil.example")
	if status != 421 || errorCode(out) != "unknown_host" {
		t.Errorf("foreign host name: %d %v", status, out)
	}
	if status, _ := f.do(t, "GET", "/api/v1/printers", nil, "Host", "platen.local"); status != 200 {
		t.Errorf(".local name: %d", status)
	}
}

func TestEvents(t *testing.T) {
	f := newFixture(t, false)
	resp, err := http.Get(f.srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	if status, _ := f.do(t, "POST", "/api/v1/scans", map[string]any{"resolution": 75}); status != 201 {
		t.Fatalf("scan: %d", status)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			if line == "event: scan" {
				return
			}
		case <-deadline:
			t.Fatal("no scan event arrived")
		}
	}
}
