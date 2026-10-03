// Package paperless is a client for the parts of the Paperless-ngx REST API that
// a print and scan hub needs: upload a document, follow its import, look up tags
// and correspondents, search, and download a document to print it.
package paperless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Paperless-ngx instance.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client. baseURL is the address of the web interface, without /api.
func New(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: httpClient}
}

// APIError is an error answer from Paperless.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("paperless: HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("paperless: HTTP %d", e.Status)
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	// Version 9 is the oldest API version Paperless-ngx 3 still serves; 2.x serves it too.
	req.Header.Set("Accept", "application/json; version=9")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paperless: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping checks that the instance answers and the token is accepted. It returns
// the Paperless-ngx version when the server reports one.
func (c *Client) Ping(ctx context.Context) (string, error) {
	resp, err := c.request(ctx, http.MethodGet, "/api/tags/?page_size=1", nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.Header.Get("X-Version"), nil
}

// Named is a tag, correspondent or document type.
type Named struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type page[T any] struct {
	Count   int    `json:"count"`
	Next    string `json:"next"`
	Results []T    `json:"results"`
}

func (c *Client) listNamed(ctx context.Context, kind string) ([]Named, error) {
	all := []Named{}
	path := "/api/" + kind + "/?page_size=250"
	for i := 0; path != "" && i < 40; i++ {
		var p page[Named]
		if err := c.getJSON(ctx, path, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Results...)
		path = ""
		if p.Next != "" {
			if u, err := url.Parse(p.Next); err == nil {
				path = u.RequestURI()
			}
		}
	}
	return all, nil
}

// Tags lists every tag.
func (c *Client) Tags(ctx context.Context) ([]Named, error) { return c.listNamed(ctx, "tags") }

// Correspondents lists every correspondent.
func (c *Client) Correspondents(ctx context.Context) ([]Named, error) {
	return c.listNamed(ctx, "correspondents")
}

// DocumentTypes lists every document type.
func (c *Client) DocumentTypes(ctx context.Context) ([]Named, error) {
	return c.listNamed(ctx, "document_types")
}

// EnsureNamed returns the id of the tag, correspondent or document type with the
// given name, creating it when it doesn't exist. kind is "tags", "correspondents"
// or "document_types". Names are compared without regard to case.
func (c *Client) EnsureNamed(ctx context.Context, kind, name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("paperless: empty name")
	}
	var p page[Named]
	if err := c.getJSON(ctx, "/api/"+kind+"/?name__iexact="+url.QueryEscape(name), &p); err != nil {
		return 0, err
	}
	if len(p.Results) > 0 {
		return p.Results[0].ID, nil
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := c.request(ctx, http.MethodPost, "/api/"+kind+"/", bytes.NewReader(body), "application/json")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var created Named
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return 0, err
	}
	return created.ID, nil
}

// Upload describes a document to file.
type Upload struct {
	Filename string
	Data     io.Reader
	Title    string
	// Created is the document's date (not the upload time). Zero lets Paperless
	// find a date in the text.
	Created       time.Time
	Correspondent int
	DocumentType  int
	Tags          []int
}

// Upload sends a document to Paperless. It returns the id of the import task;
// Paperless processes the document in the background (see WaitTask).
func (c *Client) Upload(ctx context.Context, u Upload) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="document"; filename=%q`, sanitizeFilename(u.Filename)))
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, u.Data); err != nil {
		return "", fmt.Errorf("paperless: read document: %w", err)
	}
	if u.Title != "" {
		_ = mw.WriteField("title", u.Title)
	}
	if !u.Created.IsZero() {
		_ = mw.WriteField("created", u.Created.Format("2006-01-02"))
	}
	if u.Correspondent > 0 {
		_ = mw.WriteField("correspondent", strconv.Itoa(u.Correspondent))
	}
	if u.DocumentType > 0 {
		_ = mw.WriteField("document_type", strconv.Itoa(u.DocumentType))
	}
	for _, t := range u.Tags {
		_ = mw.WriteField("tags", strconv.Itoa(t))
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	resp, err := c.request(ctx, http.MethodPost, "/api/documents/post_document/", &body, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	// The answer is the task id as a JSON string.
	var task string
	if err := json.Unmarshal(raw, &task); err != nil {
		task = strings.Trim(strings.TrimSpace(string(raw)), `"`)
	}
	if task == "" {
		return "", errors.New("paperless: upload accepted but no task id returned")
	}
	return task, nil
}

// Task is the state of a background import.
type Task struct {
	ID string `json:"id"`
	// Status is pending, started, success or failure (lower case).
	Status string `json:"status"`
	// DocumentID is set once the import succeeded.
	DocumentID int    `json:"document_id,omitempty"`
	Result     string `json:"result,omitempty"`
}

// Done reports whether the task finished, successfully or not.
func (t *Task) Done() bool { return t.Status == "success" || t.Status == "failure" }

// Task reads the state of an import task.
func (c *Client) Task(ctx context.Context, id string) (*Task, error) {
	resp, err := c.request(ctx, http.MethodGet, "/api/tasks/?task_id="+url.QueryEscape(id), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	type wire struct {
		Status          string          `json:"status"`
		Result          json.RawMessage `json:"result"`
		RelatedDocument json.RawMessage `json:"related_document"`
	}
	var list []wire
	if err := json.Unmarshal(raw, &list); err != nil {
		// Paperless-ngx 3 pages this endpoint.
		var p page[wire]
		if err2 := json.Unmarshal(raw, &p); err2 != nil {
			return nil, fmt.Errorf("paperless: decode task: %w", err)
		}
		list = p.Results
	}
	t := &Task{ID: id, Status: "pending"}
	if len(list) == 0 {
		return t, nil // not registered yet
	}
	// Paperless 2 reports SUCCESS, Paperless 3 reports success.
	t.Status = strings.ToLower(list[0].Status)
	var result string
	if json.Unmarshal(list[0].Result, &result) == nil {
		t.Result = result
	}
	var docID any
	if json.Unmarshal(list[0].RelatedDocument, &docID) == nil {
		switch v := docID.(type) {
		case float64:
			t.DocumentID = int(v)
		case string:
			t.DocumentID, _ = strconv.Atoi(v)
		}
	}
	return t, nil
}

// WaitTask polls a task until it finishes or ctx ends.
func (c *Client) WaitTask(ctx context.Context, id string) (*Task, error) {
	delay := 500 * time.Millisecond
	for {
		t, err := c.Task(ctx, id)
		if err != nil {
			return nil, err
		}
		if t.Done() {
			if t.Status == "failure" {
				return t, fmt.Errorf("paperless: import failed: %s", t.Result)
			}
			return t, nil
		}
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 3*time.Second {
			delay += 500 * time.Millisecond
		}
	}
}

// Document is a document in Paperless.
type Document struct {
	ID            int       `json:"id"`
	Title         string    `json:"title"`
	Created       string    `json:"created,omitempty"`
	Added         time.Time `json:"added,omitzero"`
	Correspondent int       `json:"correspondent,omitempty"`
	DocumentType  int       `json:"document_type,omitempty"`
	Tags          []int     `json:"tags,omitempty"`
	PageCount     int       `json:"page_count,omitempty"`
	Filename      string    `json:"original_file_name,omitempty"`
}

// Search finds documents. An empty query lists the newest documents.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Document, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	q := url.Values{"page_size": {strconv.Itoa(limit)}}
	if strings.TrimSpace(query) != "" {
		q.Set("query", query)
	} else {
		q.Set("ordering", "-added")
	}
	var raw struct {
		Count   int `json:"count"`
		Results []struct {
			ID            int             `json:"id"`
			Title         string          `json:"title"`
			Created       string          `json:"created"`
			Added         time.Time       `json:"added"`
			Correspondent *int            `json:"correspondent"`
			DocumentType  *int            `json:"document_type"`
			Tags          []int           `json:"tags"`
			PageCount     *int            `json:"page_count"`
			Filename      json.RawMessage `json:"original_file_name"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, "/api/documents/?"+q.Encode(), &raw); err != nil {
		return nil, 0, err
	}
	docs := make([]Document, 0, len(raw.Results))
	for _, r := range raw.Results {
		d := Document{ID: r.ID, Title: r.Title, Created: r.Created, Added: r.Added, Tags: r.Tags}
		if r.Correspondent != nil {
			d.Correspondent = *r.Correspondent
		}
		if r.DocumentType != nil {
			d.DocumentType = *r.DocumentType
		}
		if r.PageCount != nil {
			d.PageCount = *r.PageCount
		}
		_ = json.Unmarshal(r.Filename, &d.Filename)
		docs = append(docs, d)
	}
	return docs, raw.Count, nil
}

// Document reads one document's metadata.
func (c *Client) Document(ctx context.Context, id int) (*Document, error) {
	var raw struct {
		ID        int    `json:"id"`
		Title     string `json:"title"`
		Created   string `json:"created"`
		PageCount *int   `json:"page_count"`
	}
	if err := c.getJSON(ctx, "/api/documents/"+strconv.Itoa(id)+"/", &raw); err != nil {
		return nil, err
	}
	d := &Document{ID: raw.ID, Title: raw.Title, Created: raw.Created}
	if raw.PageCount != nil {
		d.PageCount = *raw.PageCount
	}
	return d, nil
}

// Download fetches a document's file: the archived PDF by default, or the
// original upload. The caller closes the body.
func (c *Client) Download(ctx context.Context, id int, original bool) (io.ReadCloser, string, error) {
	path := "/api/documents/" + strconv.Itoa(id) + "/download/"
	if original {
		path += "?original=true"
	}
	resp, err := c.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, "", err
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

// DocumentURL returns the address of a document in the Paperless web interface.
func DocumentURL(publicBase string, id int) string {
	return strings.TrimRight(publicBase, "/") + "/documents/" + strconv.Itoa(id) + "/details"
}

func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == '"' || r < 0x20:
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return "document.pdf"
	}
	return name
}
