package api

import "net/http"

// openAPI serves a description of the REST API (OpenAPI 3.1).
func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) {
	type m = map[string]any
	ref := func(name string) m { return m{"$ref": "#/components/schemas/" + name} }
	jsonBody := func(schema m) m { return m{"content": m{"application/json": m{"schema": schema}}} }
	ok := func(desc string, schema m) m {
		r := m{"description": desc}
		if schema != nil {
			r["content"] = m{"application/json": m{"schema": schema}}
		}
		return r
	}
	errs := m{
		"400": ok("The request is not valid.", ref("Error")), "401": ok("A token is required.", ref("Error")),
		"404": ok("No such printer, scanner, scan or job.", ref("Error")), "502": ok("The device did not cooperate.", ref("Error")),
	}
	with := func(responses m) m {
		for k, v := range errs {
			if _, has := responses[k]; !has {
				responses[k] = v
			}
		}
		return responses
	}
	id := func(name, desc string) m {
		return m{"name": name, "in": "path", "required": true, "description": desc, "schema": m{"type": "string"}}
	}
	str, integer, boolean := m{"type": "string"}, m{"type": "integer"}, m{"type": "boolean"}
	object := func(props m) m { return m{"type": "object", "properties": props} }

	printOptions := m{
		"printer": m{"type": "string", "description": "Printer id. Empty means the default printer."},
		"title":   str, "copies": m{"type": "integer", "minimum": 1, "default": 1},
		"duplex":  m{"type": "string", "enum": []string{"off", "long-edge", "short-edge"}, "default": "off"},
		"color":   m{"type": "string", "enum": []string{"auto", "color", "monochrome"}, "default": "auto"},
		"quality": m{"type": "string", "enum": []string{"draft", "normal", "high"}, "default": "normal"},
		"media":   m{"type": "string", "description": "Paper size: a4, letter, legal, a5, 4x6, 5x7 or a PWG name such as iso_a4_210x297mm."},
		"pages":   m{"type": "string", "description": `Pages to print, e.g. "1-3,5". Empty prints everything.`},
		"fill":    m{"type": "boolean", "description": "Make a picture cover the sheet, cropping what doesn't fit."},
		"confirm": m{"type": "boolean", "description": "Acknowledge a job above the configured sheet threshold."},
		"dry_run": m{"type": "boolean", "description": "Report pages and sheets without printing."},
	}
	jsonPrint := m{}
	for k, v := range printOptions {
		jsonPrint[k] = v
	}
	jsonPrint["source"] = ref("Source")
	formPrint := m{"file": m{"type": "string", "format": "binary"}, "url": str, "text": str, "paperless_id": integer, "scan_id": str}
	for k, v := range printOptions {
		formPrint[k] = v
	}

	doc := m{
		"openapi": "3.1.0",
		"info": m{
			"title": "Platen", "version": s.version,
			"description": "Print, scan and file documents. Every endpoint needs `Authorization: Bearer <token>` when access tokens are configured.",
			"license":     m{"name": "MIT"},
		},
		"servers":  []m{{"url": s.cfg.Server.BaseURL}},
		"security": []m{{"bearer": []string{}}},
		"paths": m{
			"/api/v1/info":                   m{"get": m{"summary": "This Platen instance", "responses": m{"200": ok("Version, devices and limits.", nil)}}},
			"/api/v1/printers":               m{"get": m{"summary": "Printers with their live state and ink levels", "responses": with(m{"200": ok("Printers.", object(m{"printers": m{"type": "array", "items": ref("Printer")}}))})}},
			"/api/v1/printers/{id}":          m{"get": m{"summary": "One printer", "parameters": []m{id("id", "Printer id")}, "responses": with(m{"200": ok("The printer.", ref("Printer"))})}},
			"/api/v1/printers/{id}/identify": m{"post": m{"summary": "Make the printer flash or beep", "parameters": []m{id("id", "Printer id")}, "responses": with(m{"200": ok("Done.", nil)})}},
			"/api/v1/printers/{id}/queue": m{"get": m{"summary": "Jobs the printer itself reports", "parameters": []m{id("id", "Printer id"),
				{"name": "which", "in": "query", "schema": m{"type": "string", "enum": []string{"not-completed", "completed"}}}}, "responses": with(m{"200": ok("Jobs.", nil)})}},
			"/api/v1/printers/{id}/queue/{job}": m{"delete": m{"summary": "Cancel a job by the printer's job number", "parameters": []m{id("id", "Printer id"), id("job", "Printer job number")}, "responses": with(m{"200": ok("Cancelled.", nil)})}},
			"/api/v1/print": m{"post": m{
				"summary":     "Print a document",
				"description": "Send a file as multipart/form-data, or JSON naming a URL, a Paperless document, a scan, a path or text. Answers 409 with code `confirmation_required` when the job needs more sheets than the configured threshold; repeat with `confirm: true`. If the printer rejects the job as busy, answers 409 with code `busy`; wait for the printer before retrying.",
				"requestBody": m{"required": true, "content": m{
					"multipart/form-data": m{"schema": object(formPrint)},
					"application/json":    m{"schema": object(jsonPrint)},
				}},
				"responses": with(m{"200": ok("The job was sent (or, for a dry run, checked).", ref("PrintResult")), "409": ok("Confirmation required or the printer is busy.", ref("Error")), "422": ok("The printer can't print this.", ref("Error"))}),
			}},
			"/api/v1/jobs":      m{"get": m{"summary": "Print history", "responses": with(m{"200": ok("Jobs, newest first.", nil)})}},
			"/api/v1/jobs/{id}": m{"delete": m{"summary": "Cancel a job from the history", "parameters": []m{id("id", "Job id")}, "responses": with(m{"200": ok("The job.", nil)})}},
			"/api/v1/scanners":  m{"get": m{"summary": "Scanners with their state and capabilities", "responses": with(m{"200": ok("Scanners.", nil)})}},
			"/api/v1/scans": m{
				"get": m{"summary": "Scans, newest first", "responses": with(m{"200": ok("Scans.", nil)})},
				"post": m{"summary": "Start a scan", "description": "Scans the page on the glass, or every page in the feeder. Returns the scan with its pages.",
					"requestBody": jsonBody(ref("ScanRequest")), "responses": with(m{"201": ok("The scan.", ref("Scan")), "409": ok("The scanner is busy.", ref("Error"))})},
			},
			"/api/v1/scans/{id}": m{
				"get":    m{"summary": "One scan", "parameters": []m{id("id", "Scan id")}, "responses": with(m{"200": ok("The scan.", ref("Scan"))})},
				"patch":  m{"summary": "Rename a scan", "parameters": []m{id("id", "Scan id")}, "requestBody": jsonBody(object(m{"title": str})), "responses": with(m{"200": ok("The scan.", ref("Scan"))})},
				"delete": m{"summary": "Delete a scan and its files", "parameters": []m{id("id", "Scan id")}, "responses": with(m{"200": ok("Deleted.", nil)})},
			},
			"/api/v1/scans/{id}/pages":        m{"post": m{"summary": "Scan another page into the same document", "parameters": []m{id("id", "Scan id")}, "responses": with(m{"201": ok("The scan.", ref("Scan"))})}},
			"/api/v1/scans/{id}/pages/{page}": m{"delete": m{"summary": "Remove a page", "parameters": []m{id("id", "Scan id"), id("page", "Page id")}, "responses": with(m{"200": ok("The scan.", ref("Scan"))})}},
			"/api/v1/scans/{id}/pages/{page}/move": m{"post": m{"summary": "Move a page", "parameters": []m{id("id", "Scan id"), id("page", "Page id")},
				"requestBody": jsonBody(object(m{"to": m{"type": "integer", "description": "New position, starting at 0."}})), "responses": with(m{"200": ok("The scan.", ref("Scan"))})}},
			"/api/v1/scans/{id}/pages/{page}/image": m{"get": m{"summary": "A page as JPEG", "parameters": []m{id("id", "Scan id"), id("page", "Page id"),
				{"name": "size", "in": "query", "description": "Longest side in pixels; omit for the full scan.", "schema": integer}},
				"responses": m{"200": m{"description": "The picture.", "content": m{"image/jpeg": m{}}}}}},
			"/api/v1/scans/{id}/finish": m{"post": m{"summary": "Build the document of a scan", "parameters": []m{id("id", "Scan id")},
				"requestBody": jsonBody(object(m{"format": m{"type": "string", "enum": []string{"pdf", "jpeg"}}})), "responses": with(m{"200": ok("The scan.", ref("Scan"))})}},
			"/api/v1/scans/{id}/document": m{"get": m{"summary": "Download the document of a scan", "parameters": []m{id("id", "Scan id")},
				"responses": m{"200": m{"description": "PDF or JPEG.", "content": m{"application/pdf": m{}, "image/jpeg": m{}}}}}},
			"/api/v1/scans/{id}/paperless": m{"post": m{"summary": "File a scan in Paperless-ngx", "parameters": []m{id("id", "Scan id")},
				"requestBody": jsonBody(ref("FileRequest")), "responses": with(m{"200": ok("The scan, with its filing state.", ref("Scan"))})}},
			"/api/v1/scans/{id}/print": m{"post": m{"summary": "Print a scan (a photocopy)", "parameters": []m{id("id", "Scan id")},
				"requestBody": jsonBody(object(printOptions)), "responses": with(m{"200": ok("The job.", ref("PrintResult")), "409": ok("Confirmation required or the printer is busy.", ref("Error"))})}},
			"/api/v1/paperless": m{"get": m{"summary": "Tags, correspondents and document types of the Paperless instance", "responses": with(m{"200": ok("Lists.", nil)})}},
			"/api/v1/paperless/documents": m{"get": m{"summary": "Search Paperless", "parameters": []m{{"name": "query", "in": "query", "schema": str}, {"name": "limit", "in": "query", "schema": integer}},
				"responses": with(m{"200": ok("Documents.", nil)})}},
			"/api/v1/events": m{"get": m{"summary": "Live updates (server-sent events)", "description": "Event types: job, scan, scan.deleted.",
				"responses": m{"200": m{"description": "An event stream.", "content": m{"text/event-stream": m{}}}}}},
		},
		"components": m{
			"securitySchemes": m{"bearer": m{"type": "http", "scheme": "bearer"}},
			"schemas": m{
				"Error": object(m{"error": object(m{"code": str, "message": str, "details": m{}})}),
				"Source": m{"type": "object", "description": "Where the document comes from. Use exactly one field.", "properties": m{
					"url": m{"type": "string", "format": "uri"}, "base64": m{"type": "string", "description": "The document, base64 encoded."},
					"name": str, "paperless_id": integer, "scan_id": str, "text": str,
					"path": m{"type": "string", "description": "A file inside a directory listed in fetch.allowed_dirs."},
				}},
				"PrintResult": object(m{"id": str, "printer": str, "printer_job_id": integer, "title": str, "document_pages": integer, "pages": integer,
					"copies": integer, "sheets": integer, "duplex": str, "color": str, "quality": str, "media": str, "format": str, "state": str, "dry_run": boolean, "message": str}),
				"Printer": object(m{"id": str, "name": str, "default": boolean, "online": boolean, "error": str, "summary": str, "state": str,
					"state_reasons": m{"type": "array", "items": str}, "make_and_model": str, "queued_jobs": integer, "formats": m{"type": "array", "items": str},
					"markers": m{"type": "array", "items": object(m{"name": str, "type": str, "color": str, "level": integer, "low": integer})}}),
				"ScanRequest": object(m{"scanner": str, "source": m{"type": "string", "enum": []string{"flatbed", "feeder"}}, "duplex": boolean,
					"color": m{"type": "string", "enum": []string{"color", "gray"}}, "resolution": m{"type": "integer", "default": 300},
					"paper": m{"type": "string", "description": "a4, letter, legal, a5, full or WxH in millimetres.", "default": "a4"}, "title": str}),
				"Scan": object(m{"id": str, "scanner": str, "title": str, "source": str, "color": str, "dpi": integer, "paper": str,
					"pages":      m{"type": "array", "items": object(m{"id": str, "width": integer, "height": integer, "bytes": integer})},
					"document":   object(m{"format": str, "bytes": integer, "pages": integer}),
					"paperless":  object(m{"task_id": str, "document_id": integer, "url": str, "status": str, "message": str}),
					"created_at": m{"type": "string", "format": "date-time"}, "updated_at": m{"type": "string", "format": "date-time"}}),
				"FileRequest": object(m{"title": str, "tags": m{"type": "array", "items": str, "description": "Tag names; missing ones are created."},
					"correspondent": str, "document_type": str, "created": m{"type": "string", "format": "date"},
					"wait": m{"type": "boolean", "description": "Wait until Paperless has imported the document."}}),
			},
		},
	}
	writeJSON(w, http.StatusOK, doc)
}
