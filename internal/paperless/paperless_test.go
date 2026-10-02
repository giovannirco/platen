package paperless_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/giovannirco/platen/internal/paperless"
	"github.com/giovannirco/platen/internal/testutil"
)

func TestClient(t *testing.T) {
	fake := testutil.NewPaperless(t)
	ctx := context.Background()
	c := paperless.New(fake.URL+"/", testutil.PaperlessToken, nil)
	if _, err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	tags, err := c.Tags(ctx)
	if err != nil || len(tags) != 1 || tags[0].Name != "bills" {
		t.Fatalf("tags: %v %v", tags, err)
	}
	if id, err := c.EnsureNamed(ctx, "tags", "BILLS"); err != nil || id != 1 {
		t.Errorf("existing tag, other case: %d %v", id, err)
	}
	newTag, err := c.EnsureNamed(ctx, "tags", " taxes ")
	if err != nil || newTag == 1 {
		t.Fatalf("new tag: %d %v", newTag, err)
	}
	if again, _ := c.EnsureNamed(ctx, "tags", "taxes"); again != newTag {
		t.Errorf("the tag was created twice: %d and %d", newTag, again)
	}

	task, err := c.Upload(ctx, paperless.Upload{
		Filename: `scan "one".pdf`, Data: bytes.NewReader([]byte("%PDF-1.4 test")), Title: "Tax return",
		Created: time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), Tags: []int{1, newTag}, Correspondent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := c.WaitTask(ctx, task)
	if err != nil || done.Status != "success" || done.DocumentID == 0 {
		t.Fatalf("task: %+v %v", done, err)
	}
	doc := fake.Docs()[0]
	if doc.Title != "Tax return" || doc.Created != "2026-04-30" || len(doc.Tags) != 2 || doc.Correspondent != 1 || doc.Filename != "scan _one_.pdf" {
		t.Errorf("uploaded: %+v", doc)
	}
	docs, total, err := c.Search(ctx, "tax", 5)
	if err != nil || total != 1 || docs[0].ID != done.DocumentID || docs[0].PageCount != 1 {
		t.Errorf("search: %+v %d %v", docs, total, err)
	}
	body, contentType, err := c.Download(ctx, done.DocumentID, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "%PDF-1.4 test" || contentType != "application/pdf" {
		t.Errorf("download: %q %q", data, contentType)
	}
	if url := paperless.DocumentURL("http://paperless.lan/", 7); url != "http://paperless.lan/documents/7/details" {
		t.Errorf("document url: %s", url)
	}

	fake.FailImport = true
	task, _ = c.Upload(ctx, paperless.Upload{Filename: "x.pdf", Data: bytes.NewReader([]byte("x"))})
	if failed, err := c.WaitTask(ctx, task); err == nil || failed.Status != "failure" {
		t.Errorf("a failed import should be an error: %+v %v", failed, err)
	}

	var apiErr *paperless.APIError
	if _, err := paperless.New(fake.URL, "wrong-token", nil).Tags(ctx); !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Errorf("wrong token: %v", err)
	}
}
