package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func TestComposeFailsClosed(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"127.0.0.1:8080:8080",
		"${PLATEN_AUTH_TOKENS:?set PLATEN_AUTH_TOKENS}",
		"${PLATEN_STATE_SECRET:?set PLATEN_STATE_SECRET}",
		"read_only: true",
		"cap_drop: [ALL]",
		"no-new-privileges:true",
		`["CMD", "/usr/local/bin/platen", "healthcheck"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("compose.yaml missing %s", want)
		}
	}
	if strings.Contains(text, "0.0.0.0:") || strings.Contains(text, `"8080:8080"`) {
		t.Error("compose must not publish every interface by default")
	}
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "PLATEN_AUTH_TOKENS") {
		t.Error("README must show PLATEN_AUTH_TOKENS on docker run")
	}
}
