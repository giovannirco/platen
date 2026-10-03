package mcpserver_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCopyScanWithoutReadingDocumentResource(t *testing.T) {
	for _, mode := range []string{"stdio", "http"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			var cs *mcp.ClientSession
			if mode == "stdio" {
				cs = connect(t, f, nil)
			} else {
				cs = connectHTTP(t, f, nil)
			}
			res, scan := call(t, cs, "scan_document", map[string]any{"resolution": 75, "paper": "4x6"})
			if res.IsError {
				t.Fatal(textOf(res))
			}
			id := scan["scan_id"].(string)
			for pages := 1; pages <= 2; pages++ {
				res, copy := call(t, cs, "print_document", map[string]any{"scan_id": id, "dry_run": true})
				if res.IsError || copy["pages"] != float64(pages) || copy["state"] != "not-printed" {
					t.Fatalf("copy %d pages: %s %+v", pages, textOf(res), copy)
				}
				if pages == 1 {
					res, _ = call(t, cs, "scan_document", map[string]any{"scan_id": id})
					if res.IsError {
						t.Fatal(textOf(res))
					}
				}
			}
			if len(f.printer.Jobs()) != 0 {
				t.Fatal("a scan copy dry run printed")
			}
		})
	}
}
