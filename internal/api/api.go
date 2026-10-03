// Package api serves Platen over HTTP: the REST API, live events, the web
// interface and the MCP endpoint.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/web"
)

// Server wires the hub to HTTP.
type Server struct {
	hub     *hub.Hub
	cfg     *config.Config
	log     *slog.Logger
	version string
	mcp     http.Handler
	hosts   map[string]bool
}

// New returns a Server. mcp is the handler for the MCP endpoint and may be nil.
func New(h *hub.Hub, log *slog.Logger, version string, mcp http.Handler) *Server {
	cfg := h.Config()
	s := &Server{hub: h, cfg: cfg, log: log, version: version, mcp: mcp, hosts: map[string]bool{"localhost": true}}
	if u, err := url.Parse(cfg.Server.BaseURL); err == nil {
		s.hosts[strings.ToLower(u.Hostname())] = true
	}
	for _, host := range cfg.Server.AllowedHosts {
		s.hosts[strings.ToLower(host)] = true
	}
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/info", s.info)
	api.HandleFunc("GET /api/v1/printers", s.printers)
	api.HandleFunc("GET /api/v1/printers/{id}", s.printer)
	api.HandleFunc("POST /api/v1/printers/{id}/identify", s.identify)
	api.HandleFunc("GET /api/v1/printers/{id}/queue", s.queue)
	api.HandleFunc("DELETE /api/v1/printers/{id}/queue/{job}", s.cancelQueueJob)
	api.HandleFunc("POST /api/v1/print", s.print)
	api.HandleFunc("GET /api/v1/jobs", s.jobs)
	api.HandleFunc("DELETE /api/v1/jobs/{id}", s.cancelJob)
	api.HandleFunc("GET /api/v1/scanners", s.scanners)
	api.HandleFunc("GET /api/v1/scanners/{id}", s.scanner)
	api.HandleFunc("GET /api/v1/scans", s.scans)
	api.HandleFunc("POST /api/v1/scans", s.startScan)
	api.HandleFunc("GET /api/v1/scans/{id}", s.scan)
	api.HandleFunc("PATCH /api/v1/scans/{id}", s.renameScan)
	api.HandleFunc("DELETE /api/v1/scans/{id}", s.deleteScan)
	api.HandleFunc("POST /api/v1/scans/{id}/pages", s.scanNextPage)
	api.HandleFunc("DELETE /api/v1/scans/{id}/pages/{page}", s.removePage)
	api.HandleFunc("POST /api/v1/scans/{id}/pages/{page}/move", s.movePage)
	api.HandleFunc("GET /api/v1/scans/{id}/pages/{page}/image", s.pageImage)
	api.HandleFunc("POST /api/v1/scans/{id}/finish", s.finishScan)
	api.HandleFunc("GET /api/v1/scans/{id}/document", s.scanDocument)
	api.HandleFunc("POST /api/v1/scans/{id}/paperless", s.fileScan)
	api.HandleFunc("POST /api/v1/scans/{id}/print", s.printScan)
	api.HandleFunc("GET /api/v1/paperless", s.paperlessMeta)
	api.HandleFunc("GET /api/v1/paperless/documents", s.paperlessSearch)
	api.HandleFunc("GET /api/v1/events", s.events)
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint: "+r.Method+" "+r.URL.Path)
	})

	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", s.logout)
	mux.HandleFunc("GET /api/openapi.json", s.openAPI)
	mux.Handle("/api/", s.authenticate(api))
	if s.mcp != nil {
		mux.Handle("/mcp", s.authenticate(s.mcp))
	}
	mux.Handle("/", web.Handler())

	// Order matters: reject foreign hosts and cross-site writes before anything else.
	var h http.Handler = mux
	h = s.crossOrigin(h)
	h = s.guard(h)
	h = securityHeaders(h)
	return s.logRequests(h)
}

// crossOrigin decides about requests that a browser makes from another site.
//
// Requests that rely on the session cookie are refused when they come from
// another origin: that is how a web page would make a browser print on its
// behalf. A request that carries a bearer token in its header is different: no
// other site can know the token, so browser-based agents and MCP hosts on
// other origins may use the API and the MCP endpoint with it, and get the CORS
// answers they need.
func (s *Server) crossOrigin(next http.Handler) http.Handler {
	protected := http.NewCrossOriginProtection().Handler(next)
	const allowHeaders = "Authorization, Content-Type, Accept, Mcp-Protocol-Version, Mcp-Session-Id, Mcp-Method, Mcp-Name, Last-Event-ID, X-Platen-Client"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		api := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp"
		if origin == "" || !api {
			protected.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// A preflight carries no token; it only lets the browser send the real
			// request, which is checked like any other.
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
			h.Set("Access-Control-Allow-Headers", allowHeaders)
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.bearer(r) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, Mcp-Protocol-Version, Content-Disposition")
			next.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// ---- middleware --------------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/mcp" {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// access says how a request is let in.
type access int

const (
	accessDenied  access = iota // needs a token and has none
	accessToken                 // a valid bearer token or session cookie
	accessOpen                  // neither tokens nor trusted networks are configured
	accessTrusted               // the client is in a trusted network
)

type accessKey struct{}

// accessOf returns how the request was let in, as decided by guard.
func accessOf(r *http.Request) access {
	a, _ := r.Context().Value(accessKey{}).(access)
	return a
}

// classify decides how a request may be let in.
func (s *Server) classify(r *http.Request) access {
	switch {
	case s.hasToken(r):
		return accessToken
	case s.cfg.Auth.Open():
		return accessOpen
	}
	if addr, ok := s.clientAddr(r); ok && containsAddr(s.cfg.Auth.TrustedPrefixes, addr) {
		return accessTrusted
	}
	return accessDenied
}

// guard classifies every request and protects the ones that get in without a
// token against DNS rebinding.
//
// A web page on the internet can make a browser inside the network talk to
// Platen by giving its own host name Platen's address. The browser then sends
// that foreign name in the Host header, so a request that carries no token is
// only served under a name Platen knows. A request with a valid token needs no
// such check: the page can't know the token, and the session cookie is bound to
// the name it was set for.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := s.classify(r)
		if (a == accessOpen || a == accessTrusted) && r.URL.Path != "/healthz" && !s.knownHost(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, "unknown_host",
				fmt.Sprintf("Platen was reached as %q, which it does not know. Set server.base_url to this address or add the name to server.allowed_hosts. Requests that carry an access token are not checked.", hostName(r.Host)))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accessKey{}, a)))
	})
}

func hostName(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// knownHost reports whether the Host header names this Platen: an IP address,
// localhost, a .local name, the host of server.base_url or an allowed host.
func (s *Server) knownHost(hostport string) bool {
	if s.hosts["*"] {
		return true
	}
	host := hostName(hostport)
	return net.ParseIP(host) != nil || s.hosts[host] || strings.HasSuffix(host, ".local")
}

// clientAddr returns the address the request comes from. ok is false when that
// address can't be relied on, and the request must then not be trusted for it.
//
// The peer of the connection is the client, unless the peer is one of the
// configured reverse proxies: then the client is the last address in
// X-Forwarded-For that is not a proxy itself. A forwarding header from any other
// peer means a proxy stands in front that Platen was not told about. Its address
// would stand for everyone behind it, so nothing is trusted on its account.
func (s *Server) clientAddr(r *http.Request) (addr netip.Addr, ok bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	peer := ap.Addr().Unmap().WithZone("")
	forwarded := r.Header.Values("X-Forwarded-For")
	if !containsAddr(s.cfg.Server.ProxyPrefixes, peer) {
		if len(forwarded) > 0 || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-Ip") != "" {
			return peer, false
		}
		return peer, true
	}
	var chain []string
	for _, h := range forwarded {
		chain = append(chain, strings.Split(h, ",")...)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		hop, err := parseForwarded(chain[i])
		if err != nil {
			return peer, false
		}
		if !containsAddr(s.cfg.Server.ProxyPrefixes, hop) {
			return hop, true
		}
	}
	return peer, false // the proxy did not say who the client is
}

// parseForwarded reads one X-Forwarded-For entry: an address, optionally with a
// port or in brackets.
func parseForwarded(entry string) (netip.Addr, error) {
	entry = strings.TrimSpace(entry)
	if ap, err := netip.ParseAddrPort(entry); err == nil {
		return ap.Addr().Unmap().WithZone(""), nil
	}
	a, err := netip.ParseAddr(strings.Trim(entry, "[]"))
	return a.Unmap().WithZone(""), err
}

func containsAddr(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
			s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush lets server-sent events through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

const cookieName = "platen_token"

// validToken reports whether token is one of the configured tokens.
func (s *Server) validToken(token string) bool {
	ok := false
	for _, t := range s.cfg.Auth.Tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

// bearer reports whether the request carries a valid token in its Authorization header.
func (s *Server) bearer(r *http.Request) bool {
	if len(s.cfg.Auth.Tokens) == 0 {
		return false
	}
	h := r.Header.Get("Authorization")
	return len(h) > 7 && strings.EqualFold(h[:7], "bearer ") && s.validToken(strings.TrimSpace(h[7:]))
}

// hasToken reports whether the request carries a valid bearer token or session cookie.
func (s *Server) hasToken(r *http.Request) bool {
	if len(s.cfg.Auth.Tokens) == 0 {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return s.bearer(r)
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return s.validToken(c.Value)
	}
	return false
}

// authenticate lets a request through when guard found a reason to.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accessOf(r) == accessDenied {
			w.Header().Set("WWW-Authenticate", `Bearer realm="platen"`)
			message := "a valid access token is required (Authorization: Bearer <token>)"
			if len(s.cfg.Auth.Tokens) == 0 {
				message = "this Platen only serves its trusted networks (auth.trusted_networks), and no access token is configured"
			}
			writeError(w, http.StatusUnauthorized, "unauthorized", message)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !s.validToken(body.Token) {
		if s.cfg.Auth.Open() {
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) // nothing to log in to
			return
		}
		time.Sleep(500 * time.Millisecond) // slow down guessing
		writeError(w, http.StatusUnauthorized, "unauthorized", "that token is not valid")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: body.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"), MaxAge: 180 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- helpers -----------------------------------------------------------------

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message}})
}

// fail maps an error from the hub to an HTTP answer.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var need *hub.ConfirmationRequired
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &need):
		writeJSON(w, http.StatusConflict, map[string]apiError{"error": {Code: "confirmation_required", Message: need.Error(), Details: need}})
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the upload is larger than %d MB", s.cfg.Limits.MaxUploadMB))
	case errors.Is(err, hub.ErrNotFound), errors.Is(err, hub.ErrNoPrinter), errors.Is(err, hub.ErrNoScanner), errors.Is(err, hub.ErrNoPaperless):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, hub.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, hub.ErrNotAllowed):
		writeError(w, http.StatusForbidden, "not_allowed", err.Error())
	case errors.Is(err, hub.ErrUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "unsupported", err.Error())
	case errors.Is(err, hub.ErrLimitExceeded):
		writeError(w, http.StatusUnprocessableEntity, "limit_exceeded", err.Error())
	case errors.Is(err, hub.ErrScannerBusy), errors.Is(err, hub.ErrPrinterBusy):
		writeError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "canceled", "the request was canceled")
	default:
		s.log.Warn("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusBadGateway, "device_error", err.Error())
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid", "request body: "+err.Error())
		return false
	}
	// Validate the whole body before a handler can start a print or scan.
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid", "request body must contain only one JSON value")
		return false
	}
	return true
}

func queryInt(r *http.Request, name string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return n
	}
	return def
}

func via(r *http.Request) string {
	if r.Header.Get("X-Platen-Client") == "web" {
		return "web"
	}
	return "api"
}

// ---- general -----------------------------------------------------------------

// Info describes this Platen instance.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	BaseURL string `json:"base_url"`
	MCPURL  string `json:"mcp_url"`
	// Auth is true when access tokens are configured. Trusted is true when this
	// request was let in without one because it comes from a trusted network.
	Auth         bool             `json:"auth"`
	Trusted      bool             `json:"trusted"`
	Paperless    bool             `json:"paperless"`
	PaperlessURL string           `json:"paperless_url,omitempty"`
	Limits       config.Limits    `json:"limits"`
	Printers     []config.Printer `json:"printers"`
	Scanners     []config.Scanner `json:"scanners"`
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	info := Info{
		Name: "Platen", Version: s.version, BaseURL: s.cfg.Server.BaseURL, MCPURL: s.cfg.Server.BaseURL + "/mcp",
		Auth: len(s.cfg.Auth.Tokens) > 0, Trusted: accessOf(r) == accessTrusted,
		Paperless: s.hub.PaperlessEnabled(), Limits: s.cfg.Limits,
	}
	// Copies, so that hiding the device addresses below doesn't change the configuration.
	info.Printers = append(info.Printers, s.cfg.Printers...)
	info.Scanners = append(info.Scanners, s.cfg.Scanners...)
	if info.Paperless {
		info.PaperlessURL = s.cfg.Paperless.PublicURL
	}
	// Device addresses can carry credentials; show only the host.
	for i := range info.Printers {
		info.Printers[i].URI = hostOnly(info.Printers[i].URI)
	}
	for i := range info.Scanners {
		info.Scanners[i].URL = hostOnly(info.Scanners[i].URL)
	}
	writeJSON(w, http.StatusOK, info)
}

func hostOnly(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return ""
}

// ---- printers ----------------------------------------------------------------

func (s *Server) printers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"printers": s.hub.Printers(r.Context())})
}

func (s *Server) printer(w http.ResponseWriter, r *http.Request) {
	p, err := s.hub.Printer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) identify(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.IdentifyPrinter(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.hub.QueueJobs(r.Context(), r.PathValue("id"), r.URL.Query().Get("which"), queryInt(r, "limit", 50))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) cancelQueueJob(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("job"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "the job number must be an integer")
		return
	}
	if err := s.hub.CancelQueueJob(r.Context(), r.PathValue("id"), n); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// print accepts a multipart form (field "file" plus options) or a JSON PrintRequest.
func (s *Server) print(w http.ResponseWriter, r *http.Request) {
	var req hub.PrintRequest
	limit := int64(s.cfg.Limits.MaxUploadMB)<<20 + 1<<20
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mediaType {
	case "multipart/form-data":
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			s.fail(w, r, fmt.Errorf("%w: upload: %w", hub.ErrInvalid, err))
			return
		}
		defer r.MultipartForm.RemoveAll()
		form := r.FormValue
		req = hub.PrintRequest{
			Printer: form("printer"), Title: form("title"), Duplex: form("duplex"), Color: form("color"),
			Quality: form("quality"), Media: form("media"), Pages: form("pages"),
			Fill: truthy(form("fill")), Confirm: truthy(form("confirm")), DryRun: truthy(form("dry_run")),
		}
		req.Copies, _ = strconv.Atoi(form("copies"))
		req.Source = hub.Source{URL: form("url"), Text: form("text"), ScanID: form("scan_id")}
		req.Source.PaperlessID, _ = strconv.Atoi(form("paperless_id"))
		if file, header, err := r.FormFile("file"); err == nil {
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			req.Source = hub.Source{Data: data, Name: header.Filename, MIME: header.Header.Get("Content-Type")}
		}
	default:
		r.Body = http.MaxBytesReader(w, r.Body, limit*4/3+1<<20) // base64 inflates by a third
		if !readJSON(w, r, &req) {
			return
		}
	}
	req.Via = via(r)
	res, err := s.hub.Print(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.hub.History(queryInt(r, "limit", 50))})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.hub.CancelJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// ---- scanners and scans ------------------------------------------------------

func (s *Server) scanners(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"scanners": s.hub.Scanners(r.Context())})
}

func (s *Server) scanner(w http.ResponseWriter, r *http.Request) {
	sc, err := s.hub.Scanner(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) scans(w http.ResponseWriter, r *http.Request) {
	list, err := s.hub.Scans(queryInt(r, "limit", 50))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scans": list})
}

func (s *Server) startScan(w http.ResponseWriter, r *http.Request) {
	var req hub.ScanRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Via = via(r)
	sc, err := s.hub.StartScan(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	sc, err := s.hub.Scan(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) renameScan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	sc, err := s.hub.RenameScan(r.PathValue("id"), body.Title)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) deleteScan(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.DeleteScan(r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) scanNextPage(w http.ResponseWriter, r *http.Request) {
	sc, err := s.hub.ScanNextPage(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Server) removePage(w http.ResponseWriter, r *http.Request) {
	sc, err := s.hub.RemoveScanPage(r.PathValue("id"), r.PathValue("page"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) movePage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To int `json:"to"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	sc, err := s.hub.MoveScanPage(r.PathValue("id"), r.PathValue("page"), body.To)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) pageImage(w http.ResponseWriter, r *http.Request) {
	data, err := s.hub.PageImage(r.PathValue("id"), r.PathValue("page"), queryInt(r, "size", 0))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	_, _ = w.Write(data)
}

func (s *Server) finishScan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Format string `json:"format"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	sc, err := s.hub.FinishScan(r.PathValue("id"), body.Format)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) scanDocument(w http.ResponseWriter, r *http.Request) {
	sc, err := s.hub.Scan(r.PathValue("id"))
	if err == nil && (sc.Document == nil || r.URL.Query().Get("format") != "") {
		sc, err = s.hub.FinishScan(r.PathValue("id"), r.URL.Query().Get("format"))
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	disposition := "attachment"
	if r.URL.Query().Get("inline") != "" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": hub.ScanFilename(sc)}))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, sc.Document.File)
}

func (s *Server) fileScan(w http.ResponseWriter, r *http.Request) {
	var req hub.FileRequest
	if !readJSON(w, r, &req) {
		return
	}
	sc, err := s.hub.FileScan(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

// printScan prints a scan: together with a scan, this is a photocopy.
func (s *Server) printScan(w http.ResponseWriter, r *http.Request) {
	var req hub.PrintRequest
	if !readJSON(w, r, &req) {
		return
	}
	if _, err := s.hub.FinishScan(r.PathValue("id"), ""); err != nil {
		s.fail(w, r, err)
		return
	}
	req.Source = hub.Source{ScanID: r.PathValue("id")}
	req.Via = via(r)
	res, err := s.hub.Print(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- Paperless ---------------------------------------------------------------

func (s *Server) paperlessMeta(w http.ResponseWriter, r *http.Request) {
	meta, err := s.hub.PaperlessMeta(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (s *Server) paperlessSearch(w http.ResponseWriter, r *http.Request) {
	docs, total, err := s.hub.PaperlessSearch(r.Context(), r.URL.Query().Get("query"), queryInt(r, "limit", 20))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "documents": docs})
}

// ---- events ------------------------------------------------------------------

// events streams hub events as server-sent events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_streaming", "this connection can't stream events")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, cancel := s.hub.Subscribe()
	defer cancel()
	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
		case e := <-ch:
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
		}
		flusher.Flush()
	}
}
