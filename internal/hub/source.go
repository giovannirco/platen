package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/giovannirco/platen/internal/config"
)

// Source says where the document to print comes from. Exactly one field group is used.
type Source struct {
	// Data is the document itself (an upload). Name and MIME are optional hints.
	Data []byte `json:"-"`
	Name string `json:"name,omitempty"`
	MIME string `json:"mime,omitempty"`
	// Base64 carries the document in JSON requests.
	Base64 string `json:"base64,omitempty"`
	// URL is fetched by the server.
	URL string `json:"url,omitempty"`
	// Path is a file on the machine Platen runs on, inside an allowed directory.
	Path string `json:"path,omitempty"`
	// PaperlessID is a document in Paperless-ngx.
	PaperlessID int `json:"paperless_id,omitempty"`
	// ScanID is a finished scan held by Platen.
	ScanID string `json:"scan_id,omitempty"`
	// Text is plain text to print as it is.
	Text string `json:"text,omitempty"`
}

// document is a loaded source.
type document struct {
	name string
	kind string // pdf, jpeg, image, text
	mime string
	data []byte
	from string // upload, url, file, paperless, scan, text
}

func (h *Hub) load(ctx context.Context, src Source) (*document, error) {
	set := 0
	for _, used := range []bool{len(src.Data) > 0 || src.Base64 != "", src.URL != "", src.Path != "", src.PaperlessID > 0, src.ScanID != "", src.Text != ""} {
		if used {
			set++
		}
	}
	switch {
	case set == 0:
		return nil, invalid("nothing to print: give a file, a URL, a path, a Paperless document id, a scan id or text")
	case set > 1:
		return nil, invalid("more than one document source given; use exactly one")
	}
	limit := int64(h.cfg.Limits.MaxUploadMB) << 20

	var (
		data           []byte
		name, mimeType string
		from           = "upload"
		err            error
	)
	switch {
	case src.Text != "":
		return &document{name: orDefault(src.Name, "text"), kind: "text", mime: "text/plain", data: []byte(src.Text), from: "text"}, nil
	case src.Base64 != "":
		data, err = base64.StdEncoding.DecodeString(strings.TrimSpace(src.Base64))
		if err != nil {
			return nil, invalid("base64 document: %v", err)
		}
		name, mimeType = src.Name, src.MIME
	case len(src.Data) > 0:
		data, name, mimeType = src.Data, src.Name, src.MIME
	case src.URL != "":
		from = "url"
		data, name, mimeType, err = h.fetcher.get(ctx, src.URL)
		if err != nil {
			return nil, err
		}
	case src.Path != "":
		from = "file"
		data, name, err = h.readAllowedFile(src.Path, limit)
		if err != nil {
			return nil, err
		}
	case src.PaperlessID > 0:
		from = "paperless"
		if h.paperless == nil {
			return nil, ErrNoPaperless
		}
		body, ct, err := h.paperless.Download(ctx, src.PaperlessID, false)
		if err != nil {
			return nil, fmt.Errorf("paperless document %d: %w", src.PaperlessID, err)
		}
		defer body.Close()
		data, err = readLimited(body, limit)
		if err != nil {
			return nil, err
		}
		name, mimeType = fmt.Sprintf("paperless-%d", src.PaperlessID), ct
	case src.ScanID != "":
		from = "scan"
		sc, err := h.store.Scan(src.ScanID)
		if err != nil {
			return nil, fmt.Errorf("%w: scan %q", ErrNotFound, src.ScanID)
		}
		if sc.Document == nil {
			return nil, invalid("scan %s has no finished document yet; finish it first", src.ScanID)
		}
		data, err = os.ReadFile(sc.Document.File)
		if err != nil {
			return nil, err
		}
		name = orDefault(sc.Title, sc.ID)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: the document is %d MB, the limit is %d MB", ErrLimitExceeded, len(data)>>20, h.cfg.Limits.MaxUploadMB)
	}
	if len(data) == 0 {
		return nil, invalid("the document is empty")
	}
	kind, detected := sniff(data, mimeType)
	switch {
	case kind == "" && (strings.HasPrefix(detected, "text/html") || strings.HasPrefix(detected, "text/xml")):
		return nil, fmt.Errorf("%w: this is a web page (%s), which Platen can't lay out; save or export it as PDF and print that", ErrUnsupported, strings.SplitN(detected, ";", 2)[0])
	case kind == "":
		return nil, fmt.Errorf("%w: %s documents can't be printed; use PDF, JPEG, PNG, GIF, TIFF, WebP, BMP or plain text", ErrUnsupported, detected)
	}
	if kind == "text" && !utf8.Valid(data) {
		// Text that is not UTF-8 is almost always an old Windows or Latin-1 file.
		if converted, err := charmap.Windows1252.NewDecoder().Bytes(data); err == nil {
			data = converted
		}
	}
	return &document{name: orDefault(name, "document"), kind: kind, mime: detected, data: data, from: from}, nil
}

// sniff decides what a document is from its first bytes. The declared type is
// only used to accept plain text that doesn't look like text at first sight.
// Web pages are not text: printing their source helps nobody.
func sniff(data []byte, declared string) (kind, mimeType string) {
	if bytes.HasPrefix(data, []byte("%PDF-")) || bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) {
		return "pdf", "application/pdf"
	}
	detected := http.DetectContentType(data)
	switch {
	case detected == "image/jpeg":
		return "jpeg", detected
	case detected == "image/png", detected == "image/gif", detected == "image/bmp", detected == "image/webp":
		return "image", detected
	case bytes.HasPrefix(data, []byte("II*\x00")) || bytes.HasPrefix(data, []byte("MM\x00*")):
		return "image", "image/tiff"
	case strings.HasPrefix(detected, "text/plain"):
		return "text", "text/plain"
	case strings.HasPrefix(detected, "text/"):
		return "", detected // HTML or XML
	}
	if mt, _, err := mime.ParseMediaType(declared); err == nil && (mt == "text/plain" || mt == "text/markdown" || mt == "text/csv") && utf8.Valid(data) {
		return "text", "text/plain"
	}
	return "", detected
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: the document is larger than %d MB", ErrLimitExceeded, limit>>20)
	}
	return data, nil
}

func (h *Hub) readAllowedFile(p string, limit int64) ([]byte, string, error) {
	if len(h.cfg.Fetch.AllowedDirs) == 0 {
		return nil, "", fmt.Errorf("%w: printing files by path is turned off (fetch.allowed_dirs is empty)", ErrNotAllowed)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, "", invalid("path: %v", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	inside := ""
	for _, dir := range h.cfg.Fetch.AllowedDirs {
		root, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(root, real); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			inside = rel
			break
		}
	}
	if inside == "" {
		return nil, "", fmt.Errorf("%w: %s is outside the allowed directories", ErrNotAllowed, p)
	}
	// Hidden names below the allowed directory stay private (.ssh, .env, ...).
	for _, part := range strings.Split(inside, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return nil, "", fmt.Errorf("%w: hidden files and directories are never read", ErrNotAllowed)
		}
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, "", invalid("%s is not a regular file", p)
	}
	data, err := readLimited(f, limit)
	return data, filepath.Base(real), err
}

// fetcher downloads documents by URL without becoming a way into the private network.
type fetcher struct {
	cfg    config.Fetch
	limit  int64
	client *http.Client
}

func newFetcher(cfg config.Fetch, limit int64) *fetcher {
	f := &fetcher{cfg: cfg, limit: limit}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		// The check runs on the address actually dialled, after DNS resolution,
		// so a hostname that resolves to a private address is caught too.
		Control: func(_, address string, _ syscall.RawConn) error {
			if cfg.AllowPrivateNetworks {
				return nil
			}
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrNotAllowed, address)
			}
			if !publicAddr(ap.Addr()) {
				return fmt.Errorf("%w: %s is a private address (set fetch.allow_private_networks to allow it)", ErrNotAllowed, ap.Addr())
			}
			return nil
		},
	}
	f.client = &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			Proxy:                 nil, // a proxy would hide the real destination from the address check
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect to %s", ErrNotAllowed, req.URL.Scheme)
			}
			return nil
		},
	}
	return f
}

// notPublic lists address ranges that are not private in the strict sense but
// never hold a public web server either.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: an IPv4 address in disguise
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4: an IPv4 address in disguise
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fec0::/10"),      // old site-local
}

func publicAddr(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func (f *fetcher) get(ctx context.Context, rawURL string) (data []byte, name, mimeType string, err error) {
	if !f.cfg.AllowURLs {
		return nil, "", "", fmt.Errorf("%w: printing by URL is turned off (fetch.allow_urls)", ErrNotAllowed)
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", "", invalid("url %q: use an http or https address", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", "", invalid("url: %v", err)
	}
	req.Header.Set("User-Agent", "Platen")
	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrNotAllowed) {
			return nil, "", "", fmt.Errorf("%w: %s", ErrNotAllowed, unwrapMessage(err))
		}
		return nil, "", "", fmt.Errorf("fetch %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("fetch %s: HTTP %s", u.Redacted(), resp.Status)
	}
	data, err = readLimited(resp.Body, f.limit)
	if err != nil {
		return nil, "", "", err
	}
	name = path.Base(u.Path)
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	if name == "." || name == "/" {
		name = u.Hostname()
	}
	return data, name, resp.Header.Get("Content-Type"), nil
}

func unwrapMessage(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ErrNotAllowed.Error()+": "); i >= 0 {
		return msg[i+len(ErrNotAllowed.Error())+2:]
	}
	return msg
}
