package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// PaperlessToken is the API token the fake instance accepts.
const PaperlessToken = "test-paperless-token-0123456789"

// PaperlessDoc is a document held by the fake Paperless instance.
type PaperlessDoc struct {
	ID            int
	Title         string
	Created       string
	Filename      string
	Data          []byte
	Tags          []int
	Correspondent int
	DocumentType  int
}

// Paperless is a fake Paperless-ngx instance covering the endpoints Platen uses.
type Paperless struct {
	*httptest.Server

	mu     sync.Mutex
	names  map[string][]named // tags, correspondents, document_types
	docs   []*PaperlessDoc
	tasks  map[string]int // task id -> document id
	nextID int
	// FailImport makes every uploaded document fail in the background task.
	FailImport bool
}

type named struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// NewPaperless starts a fake instance with one tag and one correspondent.
func NewPaperless(t *testing.T) *Paperless {
	p := &Paperless{
		names: map[string][]named{
			"tags":           {{ID: 1, Name: "bills"}},
			"correspondents": {{ID: 1, Name: "Energy Company"}},
			"document_types": {},
		},
		tasks: map[string]int{}, nextID: 10,
	}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Close)
	return p
}

// Docs returns the documents uploaded so far.
func (p *Paperless) Docs() []*PaperlessDoc {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*PaperlessDoc(nil), p.docs...)
}

// AddDocument stores a document directly, as if it had been imported earlier.
func (p *Paperless) AddDocument(title string, data []byte) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	p.docs = append(p.docs, &PaperlessDoc{ID: p.nextID, Title: title, Data: data, Created: "2026-09-15"})
	return p.nextID
}

// Names returns the names of a kind: tags, correspondents or document_types.
func (p *Paperless) Names(kind string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, n := range p.names[kind] {
		out = append(out, n.Name)
	}
	return out
}

func (p *Paperless) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Token "+PaperlessToken {
		http.Error(w, `{"detail":"Invalid token."}`, http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	parts := strings.Split(path, "/")

	switch {
	case len(parts) == 1 && p.names[parts[0]] != nil || path == "document_types":
		kind := parts[0]
		if r.Method == http.MethodPost {
			var body struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.nextID++
			n := named{ID: p.nextID, Name: body.Name}
			p.names[kind] = append(p.names[kind], n)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(n)
			return
		}
		list := p.names[kind]
		if q := r.URL.Query().Get("name__iexact"); q != "" {
			var match []named
			for _, n := range list {
				if strings.EqualFold(n.Name, q) {
					match = append(match, n)
				}
			}
			list = match
		}
		if list == nil {
			list = []named{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(list), "next": nil, "results": list})

	case path == "documents/post_document" && r.Method == http.MethodPost:
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("document")
		if err != nil {
			http.Error(w, `{"document":["No file was submitted."]}`, http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		p.nextID++
		doc := &PaperlessDoc{ID: p.nextID, Title: r.FormValue("title"), Created: r.FormValue("created"), Filename: header.Filename, Data: data}
		doc.Correspondent, _ = strconv.Atoi(r.FormValue("correspondent"))
		doc.DocumentType, _ = strconv.Atoi(r.FormValue("document_type"))
		for _, t := range r.MultipartForm.Value["tags"] {
			id, _ := strconv.Atoi(t)
			doc.Tags = append(doc.Tags, id)
		}
		task := fmt.Sprintf("task-%d", doc.ID)
		if p.FailImport {
			p.tasks[task] = -1
		} else {
			p.docs = append(p.docs, doc)
			p.tasks[task] = doc.ID
		}
		_ = json.NewEncoder(w).Encode(task)

	case path == "tasks":
		id, ok := p.tasks[r.URL.Query().Get("task_id")]
		switch {
		case !ok:
			io.WriteString(w, "[]")
		case id < 0:
			io.WriteString(w, `[{"status":"failure","result":"the file is not a document","related_document":null}]`)
		default:
			// Lower case, as Paperless-ngx 3 reports it.
			fmt.Fprintf(w, `[{"status":"success","result":"Success. New document id %d created","related_document":"%d"}]`, id, id)
		}

	case path == "documents":
		q := strings.ToLower(r.URL.Query().Get("query"))
		results := []map[string]any{}
		for _, d := range p.docs {
			if q == "" || strings.Contains(strings.ToLower(d.Title), q) {
				results = append(results, map[string]any{"id": d.ID, "title": d.Title, "created": d.Created, "tags": d.Tags, "page_count": 1})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(results), "results": results})

	case len(parts) == 3 && parts[0] == "documents" && parts[2] == "download":
		id, _ := strconv.Atoi(parts[1])
		for _, d := range p.docs {
			if d.ID == id {
				w.Header().Set("Content-Type", "application/pdf")
				_, _ = w.Write(d.Data)
				return
			}
		}
		http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)

	default:
		http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
	}
}
