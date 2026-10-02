package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *server) addResources(srv *mcp.Server) {
	// Interactive views for hosts that support MCP Apps. They are plain HTML
	// files with their script and style inline, so they need no network access.
	for _, v := range []struct{ uri, file, name, desc string }{
		{uiPrinter, "ui/printer-status.html", "Printer status", "Ink levels, paper and queue of a printer"},
		{uiScan, "ui/scan.html", "Scan", "The scanned pages, with buttons to add a page or file the document"},
	} {
		html, err := uiFiles.ReadFile(v.file)
		if err != nil {
			panic(err)
		}
		meta := mcp.Meta{"ui": map[string]any{"prefersBorder": true}}
		srv.AddResource(&mcp.Resource{URI: v.uri, Name: v.name, Description: v.desc, MIMEType: uiMIME, Meta: meta},
			func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: uiMIME, Text: string(html), Meta: meta}}}, nil
			})
	}

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "platen://scans/{scan_id}/document", Name: "scan-document", Title: "Scanned document",
		Description: "The PDF of a scan made with scan_document", MIMEType: "application/pdf",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		id, rest, ok := strings.Cut(strings.TrimPrefix(req.Params.URI, "platen://scans/"), "/")
		if !ok || rest != "document" {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		sc, err := s.hub.FinishScan(id, "")
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		data, err := os.ReadFile(sc.Document.File)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/pdf", Blob: data}}}, nil
	})

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "platen://scans/{scan_id}/pages/{page}", Name: "scan-page", Title: "Scanned page",
		Description: "One page of a scan as a JPEG picture; page numbers start at 1", MIMEType: "image/jpeg",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		parts := strings.Split(strings.TrimPrefix(req.Params.URI, "platen://scans/"), "/")
		if len(parts) != 3 || parts[1] != "pages" {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		sc, err := s.hub.Scan(parts[0])
		n, nerr := strconv.Atoi(parts[2])
		if err != nil || nerr != nil || n < 1 || n > len(sc.Pages) {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		data, err := s.hub.PageImage(sc.ID, sc.Pages[n-1].ID, 2000)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "image/jpeg", Blob: data}}}, nil
	})

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "platen://printers/{printer_id}", Name: "printer", Title: "Printer state",
		Description: "The current state of a printer as JSON", MIMEType: "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		st, err := s.hub.Printer(ctx, strings.TrimPrefix(req.Params.URI, "platen://printers/"))
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		raw, err := json.MarshalIndent(toPrinterOut(st), "", "  ")
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(raw)}}}, nil
	})
}

func (s *server) addPrompts(srv *mcp.Server) {
	srv.AddPrompt(&mcp.Prompt{
		Name: "scan_and_file", Title: "Scan and file a document",
		Description: "Scan the page on the glass, read it, and file it in Paperless-ngx with a sensible title, tags and date.",
		Arguments:   []*mcp.PromptArgument{{Name: "hint", Title: "What it is", Description: "Anything you already know about the document, e.g. \"water bill\""}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		hint := ""
		if h := req.Params.Arguments["hint"]; h != "" {
			hint = " I can tell you this much about it: " + h + "."
		}
		return &mcp.GetPromptResult{
			Description: "Scan and file a document",
			Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "I put a document on the scanner." + hint + `

Please:
1. Scan it with scan_document (300 dpi, colour). If it has more pages, ask me to place each one and add it to the same scan.
2. Read the scanned pages and tell me in one or two sentences what the document is.
3. Propose a title, tags, a correspondent, a document type and the document's own date. Prefer tags and correspondents that already exist in Paperless.
4. When I agree, file it with file_scan_in_paperless and give me the link.`}}},
		}, nil
	})

	srv.AddPrompt(&mcp.Prompt{
		Name: "print_carefully", Title: "Print something, carefully",
		Description: "Print a document after checking the printer and how much paper the job takes.",
		Arguments:   []*mcp.PromptArgument{{Name: "what", Title: "What to print", Description: "A web address, a Paperless search, or a description of the document", Required: true}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "Print something, carefully",
			Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "Please print this: " + req.Params.Arguments["what"] + `

Before printing:
1. Check printer_status: is the printer ready, and is there enough ink?
2. Run print_document with dry_run to see how many pages and sheets it takes, and tell me.
3. Print it. Use both sides of the paper when the printer can and the document has more than two pages, unless I say otherwise.
4. Tell me when the job is done, or what went wrong.`}}},
		}, nil
	})
}
