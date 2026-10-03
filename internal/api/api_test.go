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
	cfg     *config.Config
	printer *testutil.Printer
	scanner *testutil.Scanner
	token   string
}

func newFixture(t *testing.T, withAuth bool) *fixture {
	t.Helper()
	return newFixtureWith(t, func(cfg *config.Config) {
		if withAuth {
			cfg.Auth.Tokens = []string{token}
		}
	})
}

// newFixtureWith starts Platen with fake devices. tweak may change the
// configuration before it is validated.
func newFixtureWith(t *testing.T, tweak func(*config.Config)) *fixture {
	t.Helper()
	f := &fixture{printer: testutil.NewRasterPrinter(t), scanner: testutil.NewScanner(t)}
	pl := testutil.NewPaperless(t)
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Limits.ConfirmAboveSheets = 1
	cfg.Printers = []config.Printer{{ID: "inkjet", Name: "Inkjet", URI: f.printer.URI()}}
	cfg.Scanners = []config.Scanner{{ID: "flatbed", Name: "Flatbed", URL: f.scanner.BaseURL()}}
	cfg.Paperless = config.Paperless{URL: pl.URL, Token: testutil.PaperlessToken}
	tweak(cfg)
	if len(cfg.Auth.Tokens) > 0 {
		f.token = cfg.Auth.Tokens[0]
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h, err := hub.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
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

func TestEmptyCollections(t *testing.T) {
	f := newFixture(t, false)
	for _, tc := range []struct{ path, key string }{
		{"/api/v1/scans", "scans"},
		{"/api/v1/paperless", "document_types"},
	} {
		status, out := f.do(t, "GET", tc.path, nil)
		list, ok := out[tc.key].([]any)
		if status != 200 || !ok || len(list) != 0 {
			t.Errorf("%s must return an empty array: %d %v", tc.path, status, out)
		}
	}
}

func TestPrinterRejectsBusyJob(t *testing.T) {
	f := newFixture(t, false)
	f.printer.Busy = true
	status, out := f.do(t, "POST", "/api/v1/print", map[string]any{
		"source": map[string]string{"text": "Busy printer regression"}, "quality": "draft",
	})
	if status != http.StatusConflict || errorCode(out) != "busy" {
		t.Fatalf("busy print: %d %v", status, out)
	}
	message := out["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "did not accept this job") {
		t.Errorf("unclear busy feedback: %s", message)
	}
	if len(f.printer.Jobs()) != 0 {
		t.Fatal("a rejected job was retried or accepted")
	}
	status, out = f.do(t, "GET", "/api/v1/jobs", nil)
	if status != 200 || len(out["jobs"].([]any)) != 0 {
		t.Fatalf("a rejected job appeared in history: %d %v", status, out)
	}
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
	// With tokens only, the name Platen is reached under does not matter: nothing
	// is served without the token anyway, and the login page must still load.
	f.token = ""
	if status, _ := f.do(t, "GET", "/api/v1/printers", nil, "Host", "print.example.net"); status != 401 {
		t.Errorf("no token under another name: %d", status)
	}
	req, _ = http.NewRequest("GET", f.srv.URL+"/", nil)
	req.Host = "print.example.net"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("interface under another name: %v", err)
	}
}

func TestTrustedNetwork(t *testing.T) {
	// The test client connects from 127.0.0.1.
	f := newFixtureWith(t, func(cfg *config.Config) {
		cfg.Auth.Tokens = []string{token}
		cfg.Auth.TrustedNetworks = []string{"127.0.0.0/8"}
	})
	good := f.token
	f.token = ""
	status, out := f.do(t, "GET", "/api/v1/info", nil)
	if status != 200 || out["trusted"] != true || out["auth"] != true {
		t.Fatalf("a client in a trusted network needs no token: %d %v", status, out)
	}
	// What gets in without a token is only served under a known name.
	if status, out := f.do(t, "GET", "/api/v1/printers", nil, "Host", "evil.example"); status != 421 || errorCode(out) != "unknown_host" {
		t.Errorf("trusted client, foreign host name: %d %v", status, out)
	}
	// A forwarding header means a proxy Platen was not told about stands in
	// front: its address says nothing about the client.
	if status, _ := f.do(t, "GET", "/api/v1/printers", nil, "X-Forwarded-For", "127.0.0.1"); status != 401 {
		t.Errorf("request through an undeclared proxy: %d", status)
	}
	f.token = good
	status, out = f.do(t, "GET", "/api/v1/info", nil, "Host", "evil.example", "X-Forwarded-For", "203.0.113.9")
	if status != 200 || out["trusted"] != false {
		t.Errorf("a valid token works from anywhere and under any name: %d %v", status, out)
	}
}

func TestTrustedNetworkBehindProxy(t *testing.T) {
	f := newFixtureWith(t, func(cfg *config.Config) {
		cfg.Auth.Tokens = []string{token}
		cfg.Auth.TrustedNetworks = []string{"192.168.30.0/24"}
		cfg.Server.TrustedProxies = []string{"127.0.0.1"}
	})
	f.token = ""
	for forwarded, want := range map[string]int{
		"192.168.30.7":              200, // a phone at home
		"192.168.30.7:51234":        200, // some proxies add the port
		"203.0.113.9":               401, // someone on the internet
		"192.168.30.7, 203.0.113.9": 401, // the entry the proxy added counts, not what the client claimed
		"203.0.113.9, 192.168.30.7": 200,
		"192.168.30.7, 127.0.0.1":   200, // two of our own proxies in a row
		"not-an-address":            401,
		"":                          401, // the proxy itself is never a client
	} {
		var headers []string
		if forwarded != "" {
			headers = []string{"X-Forwarded-For", forwarded}
		}
		if status, _ := f.do(t, "GET", "/api/v1/printers", nil, headers...); status != want {
			t.Errorf("X-Forwarded-For %q: got %d, want %d", forwarded, status, want)
		}
	}
}

func TestTrustedNetworkWithoutTokens(t *testing.T) {
	// Only the trusted network gets in; there is no token anyone else could use.
	f := newFixtureWith(t, func(cfg *config.Config) { cfg.Auth.TrustedNetworks = []string{"10.99.0.0/16"} })
	status, out := f.do(t, "GET", "/api/v1/printers", nil)
	if status != 401 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "trusted networks") {
		t.Errorf("client outside the trusted network: %d %v", status, out)
	}
	if status, _ := f.do(t, "POST", "/api/v1/login", map[string]any{"token": "anything-at-all-0123"}); status != 401 {
		t.Errorf("login without configured tokens: %d", status)
	}
	// The interface itself still loads, so the person sees why.
	if status, _ := f.do(t, "GET", "/healthz", nil); status != 200 {
		t.Errorf("healthz: %d", status)
	}
}

func TestInfoDoesNotChangeTheConfiguration(t *testing.T) {
	f := newFixture(t, false)
	for range 2 {
		_, out := f.do(t, "GET", "/api/v1/info", nil)
		if uri := out["printers"].([]any)[0].(map[string]any)["uri"].(string); uri == "" || strings.Contains(uri, "/ipp/") {
			t.Fatalf("printer address in info: %q", uri)
		}
	}
	// The answer hides the path of the device addresses; the configuration keeps it.
	if f.cfg.Printers[0].URI != f.printer.URI() || f.cfg.Scanners[0].URL != f.scanner.BaseURL() {
		t.Errorf("info changed the configuration: %q %q", f.cfg.Printers[0].URI, f.cfg.Scanners[0].URL)
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

func TestBrowserClientsOnOtherOriginsNeedAToken(t *testing.T) {
	f := newFixture(t, true)
	good := f.token
	f.token = ""
	cross := []string{"Origin", "https://host.example", "Sec-Fetch-Site", "cross-site"}
	// Without a token a cross-origin write is refused, with or without a cookie.
	if status, _ := f.do(t, "POST", "/api/v1/print", map[string]any{"source": map[string]any{"text": "x"}}, cross...); status != 403 {
		t.Errorf("cross-origin without token: %d", status)
	}
	// A preflight is answered so that the browser can send the real request.
	req, _ := http.NewRequest("OPTIONS", f.srv.URL+"/mcp", nil)
	req.Header.Set("Origin", "https://host.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://host.example" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %v %+v", err, resp.Header)
	}
	// With a bearer token the request goes through and gets its CORS headers.
	f.token = good
	req, _ = http.NewRequest("GET", f.srv.URL+"/api/v1/info", nil)
	req.Header.Set("Origin", "https://host.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Authorization", "Bearer "+good)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://host.example" {
		t.Errorf("cross-origin with token: %v %d %q", err, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if status, _ := f.do(t, "POST", "/api/v1/print", map[string]any{"source": map[string]any{"text": "x"}, "dry_run": true}, cross...); status != 200 {
		t.Errorf("cross-origin write with token: %d", status)
	}
	// A same-origin page gets no CORS headers and keeps working.
	if status, _ := f.do(t, "GET", "/api/v1/info", nil, "Sec-Fetch-Site", "same-origin"); status != 200 {
		t.Errorf("same-origin: %d", status)
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
