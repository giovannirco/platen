// Package mcpserver exposes Platen to AI agents through the Model Context
// Protocol.
//
// It is written for the 2026-07-28 revision of the protocol and stays usable by
// older clients:
//
//   - The HTTP endpoint is stateless: any replica can answer any request.
//   - Tools that need a person's decision (printing a lot of paper, scanning the
//     next page) ask through multi round-trip requests instead of calling the
//     client back. The state that travels with those requests is signed.
//   - Tools declare output schemas and annotations, return pictures the model
//     can look at, and link to resources instead of inlining large files.
//   - Two tools carry an interactive view (MCP Apps) for hosts that can show one.
package mcpserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giovannirco/platen/internal/hub"
)

//go:embed ui/*.html
var uiFiles embed.FS

const (
	uiExtension = "io.modelcontextprotocol/ui"
	uiMIME      = "text/html;profile=mcp-app"
	uiPrinter   = "ui://platen/printer-status.html"
	uiScan      = "ui://platen/scan.html"

	// The first protocol revision with multi round-trip requests.
	protocolMRTR = "2026-07-28"
)

const instructions = `Platen gives you a printer and a scanner, and can file documents in Paperless-ngx.

Printing uses real paper and ink:
- Call printer_status first when you are unsure whether the printer is ready or has ink.
- Use dry_run to learn how many pages and sheets a job takes before printing something large.
- Jobs above the configured sheet threshold need the person's confirmation. Platen asks them directly when your client supports it; otherwise ask them yourself and repeat the call with confirm set to true.

Scanning:
- scan_document scans what lies on the glass and returns a picture of the page, so you can read it.
- A flatbed takes one page at a time. To add pages to the same document, call scan_document again with the scan_id you got, after the person has placed the next page (or set ask_for_more_pages and Platen asks them).
- To file a scan, look at the page, propose a title, tags, correspondent and document date, check them with the person, then call file_scan_in_paperless.

Documents already in Paperless can be found with search_paperless and printed with print_document (paperless_document_id).`

// Options configure the MCP server.
type Options struct {
	Version string
	// Stateless is true for the HTTP endpoint, where the server can't call the
	// client back and every request stands alone.
	Stateless bool
	Logger    *slog.Logger
}

type server struct {
	hub       *hub.Hub
	log       *slog.Logger
	secret    []byte
	stateless bool
	baseURL   string
}

// New builds the MCP server on top of the hub.
func New(h *hub.Hub, opts Options) *mcp.Server {
	cfg := h.Config()
	s := &server{hub: h, log: opts.Logger, stateless: opts.Stateless, baseURL: cfg.Server.BaseURL}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.secret = []byte(cfg.Server.StateSecret)
	if len(s.secret) == 0 {
		s.secret = make([]byte, 32)
		if _, err := rand.Read(s.secret); err != nil {
			panic(err)
		}
	}

	caps := &mcp.ServerCapabilities{
		Tools:     &mcp.ToolCapabilities{ListChanged: false},
		Resources: &mcp.ResourceCapabilities{ListChanged: false},
		Prompts:   &mcp.PromptCapabilities{ListChanged: false},
	}
	caps.AddExtension(uiExtension, map[string]any{})

	srv := mcp.NewServer(&mcp.Implementation{
		Name:        "platen",
		Title:       "Platen",
		Description: "Print, scan and file documents in Paperless-ngx.",
		Version:     opts.Version,
		WebsiteURL:  "https://github.com/giovannirco/platen",
	}, &mcp.ServerOptions{
		Instructions: instructions,
		Logger:       s.log,
		Capabilities: caps,
		// The tool, prompt and resource lists only change with the configuration,
		// so clients may keep them for a while.
		SetCacheable: func(_ context.Context, req mcp.Request, c *mcp.Cacheable) {
			switch req.(type) {
			case *mcp.ListToolsRequest, *mcp.ListPromptsRequest, *mcp.ListResourcesRequest, *mcp.ListResourceTemplatesRequest:
				c.TTLMs, c.CacheScope = 300_000, "public"
			}
		},
	})
	s.addPrintTools(srv)
	s.addScanTools(srv)
	s.addResources(srv)
	s.addPrompts(srv)
	return srv
}

// HTTPHandler serves the MCP server over Streamable HTTP, statelessly.
func HTTPHandler(srv *mcp.Server, log *slog.Logger) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless: true,
		Logger:    log,
		// Platen is reached under LAN addresses and names; its own host check
		// (and the access token, when set) guards against DNS rebinding.
		DisableLocalhostProtection: true,
	})
}

// ---- helpers -----------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

// readOnly marks a tool that changes nothing.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: ptr(false)}
}

// additive marks a tool that adds something (a print, a scan, a filed document)
// without destroying anything.
func additive(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
}

// reaching marks an additive tool that can also read from the outside world.
func reaching(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
}

// destructive marks a tool that removes or stops something.
func destructive(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)}
}

func uiMeta(uri string) mcp.Meta {
	return mcp.Meta{"ui": map[string]any{"resourceUri": uri}}
}

func text(format string, args ...any) []mcp.Content {
	return []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}
}

// canAsk reports whether the person can be asked a question during this call.
//
// Over stateless HTTP only clients on the 2026-07-28 revision can: they answer
// input requests by retrying the call. Older clients would need the server to
// call them back, which a stateless endpoint can't do. Over stdio the SDK
// bridges both styles.
func (s *server) canAsk(req *mcp.CallToolRequest) bool {
	caps := req.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil {
		return false
	}
	return !s.stateless || req.ProtocolVersion() >= protocolMRTR
}

// answer returns the person's reply to the input request with the given key, if
// this call is the retry that carries it.
func answer(req *mcp.CallToolRequest, key string) (*mcp.ElicitResult, bool) {
	if req.Params == nil {
		return nil, false
	}
	r, ok := req.Params.InputResponses[key]
	if !ok {
		return nil, false
	}
	res, ok := r.(*mcp.ElicitResult)
	return res, ok
}

// state is what travels with a multi round-trip request. It is signed, so a
// client can't confirm one thing and have it count for another.
type state struct {
	Kind    string `json:"k"`
	Subject string `json:"s"`
	Expires int64  `json:"e"`
}

func (s *server) sign(kind, subject string) string {
	raw, _ := json.Marshal(state{Kind: kind, Subject: subject, Expires: time.Now().Add(30 * time.Minute).Unix()})
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

var errState = errors.New("the confirmation does not belong to this request; start again")

func (s *server) verify(token, kind string) (string, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", errState
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(payload)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return "", errState
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(raw)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return "", errState
	}
	var st state
	if json.Unmarshal(raw, &st) != nil || st.Kind != kind || time.Now().Unix() > st.Expires {
		return "", errState
	}
	return st.Subject, nil
}

func fingerprint(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}
