package mcpserver_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/mcpserver"
)

// rawToolCall leaves input requests unanswered, so tests can retry the real
// stateless protocol with changed arguments or changing remote content.
func rawToolCall(t *testing.T, endpoint string, args map[string]any, state string) map[string]any {
	t.Helper()
	params := &mcp.CallToolParams{
		Name: "print_document", Arguments: args,
		Meta: mcp.Meta{
			mcp.MetaKeyProtocolVersion:    "2026-07-28",
			mcp.MetaKeyClientCapabilities: map[string]any{"elicitation": map[string]any{"form": map[string]any{}}},
		},
		RequestState: state,
	}
	if state != "" {
		params.InputResponses = mcp.InputResponseMap{"confirm_print": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "print_document")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("tool HTTP response: %d %s %v", resp.StatusCode, data, err)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			if line, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				data = []byte(line)
				break
			}
		}
	}
	var reply struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(data, &reply); err != nil || reply.Error != nil || reply.Result == nil {
		t.Fatalf("tool JSON-RPC response: %s %v", data, err)
	}
	return reply.Result
}

func printEndpoint(t *testing.T, f *fixture) string {
	t.Helper()
	srv := mcpserver.New(f.hub, mcpserver.Options{Version: "test", Stateless: true})
	ts := httptest.NewServer(mcpserver.HTTPHandler(srv, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

func confirmationState(t *testing.T, result map[string]any) string {
	t.Helper()
	state, _ := result["requestState"].(string)
	if state == "" || result["inputRequests"] == nil || result["isError"] == true {
		t.Fatalf("expected print confirmation: %v", result)
	}
	return state
}

func TestConfirmationBindsContentAndSettings(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		value     any
	}{
		{name: "unchanged"},
		{"different text, same counts", "text", "A substituted note"},
		{"color", "color", "color"},
		{"quality", "quality", "normal"},
		{"paper", "media", "a5"},
		{"duplex", "duplex", "long-edge"},
		{"fill", "fill", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.printer.Formats = []string{"application/pdf"}
			endpoint := printEndpoint(t, f)
			args := map[string]any{"text": "The approved note", "title": "Approved job", "copies": 2, "quality": "draft", "color": "monochrome", "media": "4x6"}
			state := confirmationState(t, rawToolCall(t, endpoint, args, ""))
			if tc.key != "" {
				args[tc.key] = tc.value
			}
			result := rawToolCall(t, endpoint, args, state)
			if tc.key == "" {
				if result["isError"] == true || len(f.printer.Jobs()) != 1 {
					t.Fatalf("unchanged confirmation failed: %v", result)
				}
			} else if result["isError"] != true || len(f.printer.Jobs()) != 0 {
				t.Fatalf("confirmation allowed changed content or settings: %v (jobs=%d)", result, len(f.printer.Jobs()))
			}
		})
	}
}

func TestConfirmationBindsRemoteBytes(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.Fetch.AllowPrivateNetworks = true })
	f.printer.Formats = []string{"application/pdf"}
	var fetches atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if fetches.Add(1) == 1 {
			io.WriteString(w, "The approved note")
		} else {
			io.WriteString(w, "A substituted note")
		}
	}))
	t.Cleanup(source.Close)
	endpoint := printEndpoint(t, f)
	args := map[string]any{"url": source.URL + "/note.txt", "copies": 2, "quality": "draft", "color": "monochrome", "media": "4x6"}
	state := confirmationState(t, rawToolCall(t, endpoint, args, ""))
	if got := fetches.Load(); got != 1 {
		t.Errorf("confirmation must describe one loaded document; fetched %d times", got)
	}
	result := rawToolCall(t, endpoint, args, state)
	if result["isError"] != true || len(f.printer.Jobs()) != 0 {
		t.Fatalf("confirmation allowed changed URL content: %v (jobs=%d)", result, len(f.printer.Jobs()))
	}
	if got := fetches.Load(); got != 2 {
		t.Errorf("the confirmed bytes must be checked in the submission call, without another fetch; got %d fetches", got)
	}
}

func TestConfirmationBindsSelectedPages(t *testing.T) {
	f := newFixture(t)
	f.printer.Formats = []string{"application/pdf"}
	f.printer.PageRanges = true
	endpoint := printEndpoint(t, f)
	args := map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(threePagePDF(t)),
		"pages":          "1,2", "quality": "draft", "color": "monochrome", "media": "4x6",
	}
	state := confirmationState(t, rawToolCall(t, endpoint, args, ""))
	args["pages"] = "2,3"
	result := rawToolCall(t, endpoint, args, state)
	if result["isError"] != true || len(f.printer.Jobs()) != 0 {
		t.Fatalf("confirmation allowed different pages with the same counts: %v", result)
	}
}
