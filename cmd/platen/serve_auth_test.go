package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeOpenAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen string
		yaml   string
		args   []string
		env    string
		allow  bool
	}{
		{name: "refuse non-loopback", listen: "0.0.0.0:%d"},
		{name: "refuse all interfaces", listen: ":%d"},
		{name: "loopback", listen: "127.0.0.1:%d", allow: true},
		{name: "flag", listen: "0.0.0.0:%d", args: []string{"-insecure-open"}, allow: true},
		{name: "auth.open", listen: "0.0.0.0:%d", yaml: "auth:\n  open: true\n", allow: true},
		{name: "env alias", listen: "0.0.0.0:%d", env: "1", allow: true},
		{name: "env alias does not weaken", listen: "0.0.0.0:%d", env: "0"},
		{name: "token", listen: "0.0.0.0:%d", yaml: "auth:\n  tokens: ['0123456789abcdef']\n", allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			listen := fmt.Sprintf(tc.listen, port)
			dir := t.TempDir()
			body := fmt.Sprintf("server:\n  listen: %q\n  data_dir: %q\n%s", listen, dir, tc.yaml)
			path := filepath.Join(dir, "platen.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"PLATEN_LISTEN", "PLATEN_AUTH_TOKENS", "PLATEN_TRUSTED_NETWORKS", "PLATEN_CONFIG", "PLATEN_PRINTER_URI", "PLATEN_SCANNER_URL", "PLATEN_ALLOW_OPEN"} {
				t.Setenv(key, "")
			}
			if tc.env != "" {
				t.Setenv("PLATEN_ALLOW_OPEN", tc.env)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			errCh := make(chan error, 1)
			go func() {
				errCh <- cmdServe(ctx, append([]string{"-config", path}, tc.args...))
			}()
			if !tc.allow {
				select {
				case err := <-errCh:
					if err == nil || !strings.Contains(err.Error(), "refusing to listen") {
						t.Fatalf("got %v", err)
					}
				case <-time.After(5 * time.Second):
					cancel()
					t.Fatal("serve did not refuse")
				}
				return
			}
			probe := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
			deadline := time.Now().Add(5 * time.Second)
			var last error
			for time.Now().Before(deadline) {
				resp, err := http.Get(probe)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						cancel()
						select {
						case err := <-errCh:
							if err != nil {
								t.Fatal(err)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("serve did not return after shutdown")
						}
						return
					}
					last = fmt.Errorf("HTTP %d", resp.StatusCode)
				} else {
					last = err
				}
				time.Sleep(30 * time.Millisecond)
			}
			cancel()
			t.Fatalf("server did not become ready: %v", last)
		})
	}
}

func TestLocalProbeURL(t *testing.T) {
	if got := localProbeURL(":8090", "/healthz"); got != "http://127.0.0.1:8090/healthz" {
		t.Fatal(got)
	}
	if got := localProbeURL("0.0.0.0:8090", "/readyz"); got != "http://127.0.0.1:8090/readyz" {
		t.Fatal(got)
	}
	if got := localProbeURL("[::1]:8080", "/healthz"); got != "http://[::1]:8080/healthz" {
		t.Fatal(got)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
