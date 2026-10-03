package api_test

import (
	"bytes"
	"net/http"
	"testing"
)

func TestJSONRejectsTrailingDataBeforeScanning(t *testing.T) {
	f := newFixture(t, false)
	for _, suffix := range []string{` {"resolution":600}`, ` null`, ` true`, ` garbage`} {
		status, out := f.do(t, "POST", "/api/v1/scans", bytes.NewBufferString(`{"resolution":75,"paper":"4x6"}`+suffix), "Content-Type", "application/json")
		if status != http.StatusBadRequest || errorCode(out) != "invalid" {
			t.Errorf("trailing %q: %d %v", suffix, status, out)
		}
	}
	if len(f.scanner.Requests) != 0 {
		t.Fatal("malformed JSON started a scan")
	}
	status, out := f.do(t, "GET", "/api/v1/scans", nil)
	if status != 200 || (out["scans"] != nil && len(out["scans"].([]any)) != 0) {
		t.Fatalf("malformed JSON created a scan session: %d %v", status, out)
	}
}

func TestJSONAllowsTrailingWhitespace(t *testing.T) {
	f := newFixture(t, false)
	status, out := f.do(t, "POST", "/api/v1/print", bytes.NewBufferString("{\"source\":{\"text\":\"A note\"},\"dry_run\":true}\n \t"), "Content-Type", "application/json")
	if status != http.StatusOK || out["state"] != "not-printed" {
		t.Fatalf("trailing whitespace: %d %v", status, out)
	}
	if len(f.printer.Jobs()) != 0 {
		t.Fatal("a dry run printed")
	}
}
