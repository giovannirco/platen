package mcpserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/store"
)

type scanIn struct {
	ScanID          string `json:"scan_id,omitempty" jsonschema:"continue an existing scan: scan one more page into it (other settings are ignored)"`
	Scanner         string `json:"scanner,omitempty" jsonschema:"scanner id; leave empty for the default scanner"`
	Source          string `json:"source,omitempty" jsonschema:"flatbed or feeder; default: the flatbed when there is one"`
	Duplex          bool   `json:"duplex,omitempty" jsonschema:"scan both sides (feeder only)"`
	Color           string `json:"color,omitempty" jsonschema:"color (default) or gray"`
	Resolution      int    `json:"resolution,omitempty" jsonschema:"dots per inch: 150 for speed, 300 for documents (default), 600 for photos and small print"`
	Paper           string `json:"paper,omitempty" jsonschema:"area to scan: a4 (default), letter, legal, a5, 4x6, full, or a size in millimetres such as 100x150"`
	Title           string `json:"title,omitempty" jsonschema:"a title for the document, if you already know one"`
	AskForMorePages bool   `json:"ask_for_more_pages,omitempty" jsonschema:"after each flatbed page, ask the person whether to scan another page into the same document"`
}

type pageOut struct {
	Number int `json:"number"`
	Width  int `json:"width_px"`
	Height int `json:"height_px"`
}

type filedOut struct {
	Status     string `json:"status" jsonschema:"pending, success, failure or unknown"`
	DocumentID int    `json:"document_id,omitempty"`
	URL        string `json:"url,omitempty" jsonschema:"address of the document in Paperless-ngx"`
	Message    string `json:"message,omitempty"`
}

type scanOut struct {
	ScanID      string    `json:"scan_id" jsonschema:"use it to add pages, file, print or delete this scan"`
	Title       string    `json:"title,omitempty"`
	Scanner     string    `json:"scanner"`
	PageCount   int       `json:"page_count"`
	Pages       []pageOut `json:"pages"`
	DPI         int       `json:"dpi"`
	Color       string    `json:"color"`
	DocumentURL string    `json:"document_url" jsonschema:"address where a person can download the PDF"`
	Resource    string    `json:"resource" jsonschema:"MCP resource URI of the PDF"`
	Paperless   *filedOut `json:"paperless,omitempty"`
	Preview     string    `json:"preview,omitempty" jsonschema:"small picture of the last page as a data URL, for display"`
	Message     string    `json:"message"`
}

func (s *server) scanOutput(sc *store.Scan, message string) *scanOut {
	out := &scanOut{
		ScanID: sc.ID, Title: sc.Title, Scanner: sc.Scanner, PageCount: len(sc.Pages), DPI: sc.DPI, Color: sc.Color,
		DocumentURL: s.baseURL + "/api/v1/scans/" + sc.ID + "/document", Resource: "platen://scans/" + sc.ID + "/document",
		Pages: []pageOut{}, Message: message,
	}
	for i, p := range sc.Pages {
		out.Pages = append(out.Pages, pageOut{Number: i + 1, Width: p.Width, Height: p.Height})
	}
	if f := sc.Paperless; f != nil {
		out.Paperless = &filedOut{Status: f.Status, DocumentID: f.DocumentID, URL: f.URL, Message: f.Message}
	}
	if n := len(sc.Pages); n > 0 {
		if data, err := s.hub.PageImage(sc.ID, sc.Pages[n-1].ID, 480); err == nil {
			out.Preview = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)
		}
	}
	return out
}

// scanResult builds the tool result for a scan: a sentence, pictures of the new
// pages for the model to read, a link to the PDF, and the structured summary.
func (s *server) scanResult(sc *store.Scan, firstNew int) (*mcp.CallToolResult, *scanOut, error) {
	added := len(sc.Pages) - firstNew
	msg := fmt.Sprintf("Scanned %d page(s) into scan %s (%s, %d dpi). The document now has %d page(s).", added, sc.ID, sc.Color, sc.DPI, len(sc.Pages))
	if sc.Source == "flatbed" {
		msg += " To add another page, have the next page placed on the glass and call scan_document with this scan_id."
	}
	if s.hub.PaperlessEnabled() {
		msg += " To file it, call file_scan_in_paperless."
	}
	content := []mcp.Content{&mcp.TextContent{Text: msg}}
	// The newest pages go along as pictures (at most three, to keep the result small).
	for i := max(firstNew, len(sc.Pages)-3); i < len(sc.Pages); i++ {
		if data, err := s.hub.PageImage(sc.ID, sc.Pages[i].ID, 1200); err == nil {
			content = append(content, &mcp.ImageContent{Data: data, MIMEType: "image/jpeg"})
		}
	}
	content = append(content, &mcp.ResourceLink{
		URI: "platen://scans/" + sc.ID + "/document", Name: hub.ScanFilename(sc), Title: "Scanned document (PDF)", MIMEType: "application/pdf",
	})
	return &mcp.CallToolResult{Content: content}, s.scanOutput(sc, msg), nil
}

func (s *server) scanDocument(ctx context.Context, req *mcp.CallToolRequest, in scanIn) (*mcp.CallToolResult, *scanOut, error) {
	const key = "next_page"
	var (
		sc       *store.Scan
		err      error
		firstNew int
		done     bool
	)
	if reply, ok := answer(req, key); ok {
		// The person answered "scan another page?" for a scan started earlier in this call.
		subject, err := s.verify(req.Params.RequestState, key)
		if err != nil {
			return nil, nil, err
		}
		id, first, _ := strings.Cut(subject, "|")
		firstNew, _ = strconv.Atoi(first)
		if reply.Action == "accept" && reply.Content["another"] == true {
			if sc, err = s.hub.ScanNextPage(ctx, id); err != nil {
				return nil, nil, err
			}
		} else {
			if sc, err = s.hub.Scan(id); err != nil {
				return nil, nil, err
			}
			done = true
		}
	} else if in.ScanID != "" {
		before, err := s.hub.Scan(in.ScanID)
		if err != nil {
			return nil, nil, err
		}
		firstNew = len(before.Pages)
		if sc, err = s.hub.ScanNextPage(ctx, in.ScanID); err != nil {
			return nil, nil, err
		}
	} else {
		sc, err = s.hub.StartScan(ctx, hub.ScanRequest{
			Scanner: in.Scanner, Source: in.Source, Duplex: in.Duplex, Color: in.Color, Resolution: in.Resolution,
			Paper: in.Paper, Title: in.Title, Via: "mcp",
		})
		if err != nil {
			return nil, nil, err
		}
	}

	if !done && in.AskForMorePages && sc.Source == "flatbed" && s.canAsk(req) {
		return &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{key: &mcp.ElicitParams{
				Mode:    "form",
				Message: fmt.Sprintf("Page %d is scanned. To add a page, put it on the glass and choose \"Scan another page\". Otherwise finish.", len(sc.Pages)),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"another": {Type: "boolean", Title: "Scan another page", Description: "The next page is on the glass", Default: []byte("false")},
					},
					Required: []string{"another"},
				},
			}},
			RequestState: s.sign(key, fmt.Sprintf("%s|%d", sc.ID, firstNew)),
		}, nil, nil
	}
	return s.scanResult(sc, firstNew)
}

type scansIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many recent scans to return (default 10)"`
}

type scansOut struct {
	Scans []scanOut `json:"scans" jsonschema:"scans held by Platen, newest first"`
}

type fileIn struct {
	ScanID        string   `json:"scan_id" jsonschema:"id of the scan to file"`
	Title         string   `json:"title,omitempty" jsonschema:"document title, e.g. Electricity bill September 2026"`
	Tags          []string `json:"tags,omitempty" jsonschema:"tag names; tags that do not exist yet are created"`
	Correspondent string   `json:"correspondent,omitempty" jsonschema:"who the document is from; created if new"`
	DocumentType  string   `json:"document_type,omitempty" jsonschema:"kind of document, e.g. Invoice, Contract, Letter; created if new"`
	Created       string   `json:"created,omitempty" jsonschema:"the document's own date as YYYY-MM-DD (not today's date unless it is the document's)"`
}

type searchIn struct {
	Query string `json:"query,omitempty" jsonschema:"words to look for in titles and text; leave empty for the newest documents"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many documents to return (default 10)"`
}

type docOut struct {
	ID      int    `json:"id" jsonschema:"use it as paperless_document_id to print the document"`
	Title   string `json:"title"`
	Created string `json:"created,omitempty" jsonschema:"the document's date"`
	Pages   int    `json:"pages,omitempty"`
	URL     string `json:"url" jsonschema:"address of the document in Paperless-ngx"`
}

type searchOut struct {
	Total     int      `json:"total"`
	Documents []docOut `json:"documents"`
}

type scanIDIn struct {
	ScanID string `json:"scan_id" jsonschema:"id of the scan"`
}

type deletedOut struct {
	Deleted string `json:"deleted"`
}

func (s *server) addScanTools(srv *mcp.Server) {
	scanSchema, err := jsonschema.For[scanIn](nil)
	if err != nil {
		panic(err)
	}
	scanSchema.Properties["source"].Enum = []any{"flatbed", "feeder"}
	scanSchema.Properties["color"].Enum = []any{"color", "gray"}

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "scan_document",
		Title: "Scan a document",
		Description: "Scan the page on the scanner glass (or every page in the feeder) and return it as a picture you can read, plus a PDF. " +
			"The person has to put the paper on the scanner first, so make sure they did. " +
			"A flatbed scans one page per call: call again with scan_id to add the next page to the same document.",
		Annotations: additive("Scan a document"),
		InputSchema: scanSchema,
		Meta:        uiMeta(uiScan),
	}, s.scanDocument)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_scans",
		Title:       "Recent scans",
		Description: "List the scans Platen holds, newest first, with their page count and whether they were filed in Paperless-ngx.",
		Annotations: readOnly("Recent scans"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in scansIn) (*mcp.CallToolResult, scansOut, error) {
		if in.Limit <= 0 {
			in.Limit = 10
		}
		list, err := s.hub.Scans(min(in.Limit, 50))
		if err != nil {
			return nil, scansOut{}, err
		}
		out := scansOut{Scans: []scanOut{}}
		msg := fmt.Sprintf("%d scan(s).", len(list))
		for _, sc := range list {
			o := s.scanOutput(sc, "")
			o.Preview = "" // a list does not need the pictures
			out.Scans = append(out.Scans, *o)
			filed := ""
			if sc.Paperless != nil {
				filed = ", Paperless: " + sc.Paperless.Status
			}
			msg += fmt.Sprintf("\n%s  %q, %d page(s), %s%s", sc.ID, sc.Title, len(sc.Pages), sc.CreatedAt.Format("2006-01-02 15:04"), filed)
		}
		return &mcp.CallToolResult{Content: text("%s", msg)}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "file_scan_in_paperless",
		Title: "File a scan in Paperless-ngx",
		Description: "Send a scan to Paperless-ngx as a PDF, with a title, tags, correspondent, document type and the document's date. " +
			"Read the scanned page first and propose these to the person; file once they agree. " +
			"Paperless adds searchable text itself. Returns the address of the new document.",
		Annotations: additive("File a scan in Paperless-ngx"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in fileIn) (*mcp.CallToolResult, *scanOut, error) {
		sc, err := s.hub.FileScan(ctx, in.ScanID, hub.FileRequest{
			Title: in.Title, Tags: in.Tags, Correspondent: in.Correspondent, DocumentType: in.DocumentType, Created: in.Created, Wait: true,
		})
		if err != nil {
			return nil, nil, err
		}
		f := sc.Paperless
		var msg string
		switch {
		case f == nil:
			msg = "The scan was sent to Paperless-ngx."
		case f.Status == "success":
			msg = fmt.Sprintf("Filed in Paperless-ngx as document %d: %s", f.DocumentID, f.URL)
		case f.Status == "failure":
			return nil, nil, fmt.Errorf("Paperless-ngx refused the document: %s", f.Message)
		default:
			msg = "The scan was sent to Paperless-ngx, which is still importing it."
		}
		out := s.scanOutput(sc, msg)
		out.Preview = ""
		return &mcp.CallToolResult{Content: text("%s", msg)}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "search_paperless",
		Title: "Search Paperless-ngx",
		Description: "Find documents in Paperless-ngx by words in their title or text. " +
			"Use the id of a hit with print_document (paperless_document_id) to print it.",
		Annotations: readOnly("Search Paperless-ngx"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		if in.Limit <= 0 {
			in.Limit = 10
		}
		docs, total, err := s.hub.PaperlessSearch(ctx, in.Query, min(in.Limit, 50))
		if err != nil {
			return nil, searchOut{}, err
		}
		out := searchOut{Total: total, Documents: []docOut{}}
		msg := fmt.Sprintf("%d document(s) match; showing %d.", total, len(docs))
		for _, d := range docs {
			out.Documents = append(out.Documents, docOut{ID: d.ID, Title: d.Title, Created: d.Created, Pages: d.PageCount, URL: d.URL})
			msg += fmt.Sprintf("\n#%d  %q  (%s)", d.ID, d.Title, d.Created)
		}
		return &mcp.CallToolResult{Content: text("%s", msg)}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_scan",
		Title:       "Delete a scan",
		Description: "Delete a scan and its files from Platen. A copy already filed in Paperless-ngx is not touched.",
		Annotations: destructive("Delete a scan"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in scanIDIn) (*mcp.CallToolResult, deletedOut, error) {
		if err := s.hub.DeleteScan(in.ScanID); err != nil {
			return nil, deletedOut{}, err
		}
		return &mcp.CallToolResult{Content: text("Deleted scan %s.", in.ScanID)}, deletedOut{Deleted: in.ScanID}, nil
	})
}
