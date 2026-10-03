package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/hub"
	"github.com/giovannirco/platen/internal/testutil"
	"gopkg.in/yaml.v3"
)

func TestCheckJSON(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		printerDown, scannerDown, paperlessDown bool
		badToken, empty                         bool
		failed                                  int
	}{
		{name: "online"},
		{name: "printer offline", printerDown: true, failed: 1},
		{name: "scanner offline", scannerDown: true, failed: 1},
		{name: "paperless offline", paperlessDown: true, failed: 1},
		{name: "paperless authentication", badToken: true, failed: 1},
		{name: "all offline", printerDown: true, scannerDown: true, paperlessDown: true, failed: 3},
		{name: "unconfigured", empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, printer, scanner, paperless := checkFixture(t)
			printer.Down.Store(tc.printerDown)
			if tc.scannerDown {
				scanner.Close()
			}
			if tc.paperlessDown {
				paperless.Close()
			}
			if tc.badToken {
				cfg.Paperless.Token = "invalid-test-paperless-token"
			}
			if tc.empty {
				cfg.Printers = nil
				cfg.Scanners = nil
				cfg.Paperless = config.Paperless{}
			}
			stdout, stderr, code := runCheckCLI(t, cfg, "-json")
			wantCode := 0
			if tc.failed > 0 {
				wantCode = 1
			}
			if code != wantCode {
				t.Fatalf("exit status %d, want %d; stderr: %s", code, wantCode, stderr)
			}
			var report hub.CheckResult
			dec := json.NewDecoder(bytes.NewReader(stdout))
			if err := dec.Decode(&report); err != nil {
				t.Fatalf("stdout is not a JSON report: %v\n%s", err, stdout)
			}
			if err := dec.Decode(new(any)); err != io.EOF {
				t.Fatalf("extra output after the JSON report: %v", err)
			}
			if report.OK != (tc.failed == 0) || report.FailedChecks != tc.failed {
				t.Fatalf("ok=%t, failed_checks=%d; want %d failures", report.OK, report.FailedChecks, tc.failed)
			}
			if tc.empty {
				if report.Printers == nil || len(report.Printers) != 0 || report.Scanners == nil || len(report.Scanners) != 0 || report.Paperless.Enabled {
					t.Fatalf("unconfigured services must be empty arrays and disabled: %s", stdout)
				}
			} else {
				if len(report.Printers) != 1 || len(report.Scanners) != 1 {
					t.Fatalf("missing device results: %s", stdout)
				}
				p, s, pl := report.Printers[0], report.Scanners[0], report.Paperless
				if p.ID != "inkjet" || p.Online == tc.printerDown || (p.Error != "") != tc.printerDown {
					t.Errorf("printer result: %+v", p)
				}
				if !tc.printerDown && (!p.Supports("image/pwg-raster") || p.RasterBack != "rotated" || len(p.Markers) != 4) {
					t.Errorf("missing printer capabilities: %+v", p)
				}
				if s.ID != "flatbed" || s.Online == tc.scannerDown || (s.Error != "") != tc.scannerDown {
					t.Errorf("scanner result: %+v", s)
				}
				if !tc.scannerDown && (s.State != "Idle" || len(s.Resolutions) == 0 || len(s.Sources) == 0) {
					t.Errorf("missing scanner capabilities: %+v", s)
				}
				plFailed := tc.paperlessDown || tc.badToken
				if !pl.Enabled || pl.Online == plFailed || (pl.Error != "") != plFailed || pl.URL != cfg.Paperless.PublicURL {
					t.Errorf("Paperless result: %+v", pl)
				}
				if !plFailed && (pl.TagCount != 1 || pl.CorrespondentCount != 1 || pl.DocumentTypeCount != 0) {
					t.Errorf("wrong Paperless metadata counts: %+v", pl)
				}
			}
			if tc.failed > 0 && !strings.Contains(string(stderr), fmt.Sprintf("%d check(s) failed", tc.failed)) {
				t.Errorf("missing failure summary on stderr: %s", stderr)
			}
			if tc.failed == 0 && len(stderr) != 0 {
				t.Errorf("unexpected stderr: %s", stderr)
			}
			for _, secret := range append(cfg.Auth.Tokens, cfg.Paperless.Token, cfg.Server.StateSecret) {
				if secret != "" && (bytes.Contains(stdout, []byte(secret)) || bytes.Contains(stderr, []byte(secret))) {
					t.Error("check output includes a configured secret")
				}
			}
			if len(printer.Jobs()) != 0 || len(scanner.Requests) != 0 || len(paperless.Docs()) != 0 {
				t.Error("check created a print job, scan job or Paperless document")
			}
		})
	}
}

func TestCheckText(t *testing.T) {
	cfg, printer, _, _ := checkFixture(t)
	for _, down := range []bool{false, true} {
		printer.Down.Store(down)
		stdout, stderr, code := runCheckCLI(t, cfg)
		wantCode, printerLine := 0, "✓ printer inkjet (Inkjet): Platen Test Inkjet, idle"
		if down {
			wantCode, printerLine = 1, "✗ printer inkjet (Inkjet):"
		}
		if code != wantCode {
			t.Fatalf("exit status %d, want %d; stderr: %s", code, wantCode, stderr)
		}
		for _, line := range []string{printerLine, "✓ scanner flatbed (Flatbed): Platen Test Scanner, Idle", "✓ paperless at https://paperless.example.net: 1 tag(s), 1 correspondent(s), 0 document type(s)"} {
			if !strings.Contains(string(stdout), line) {
				t.Errorf("missing text %q in output: %s", line, stdout)
			}
		}
	}
}

func checkFixture(t *testing.T) (*config.Config, *testutil.Printer, *testutil.Scanner, *testutil.Paperless) {
	t.Helper()
	p := testutil.NewRasterPrinter(t)
	s := testutil.NewScanner(t)
	pl := testutil.NewPaperless(t)
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Server.StateSecret = "test-state-secret"
	cfg.Auth.Tokens = []string{"test-access-token-0123456789"}
	cfg.Printers = []config.Printer{{ID: "inkjet", Name: "Inkjet", URI: p.URI()}}
	cfg.Scanners = []config.Scanner{{ID: "flatbed", Name: "Flatbed", URL: s.BaseURL()}}
	cfg.Paperless = config.Paperless{URL: pl.URL, PublicURL: "https://paperless.example.net", Token: testutil.PaperlessToken}
	return cfg, p, s, pl
}

// Run the real CLI entrypoint in a subprocess so exit status and stdout/stderr
// separation are tested without changing the parent process's environment.
func runCheckCLI(t *testing.T, cfg *config.Config, args ...string) ([]byte, []byte, int) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "platen.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	childArgs := append([]string{"-test.run=^TestCheckCLIProcess$", "--", "check", "-config", path}, args...)
	cmd := exec.CommandContext(ctx, exe, childArgs...)
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "PLATEN_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "PLATEN_TEST_CHECK_PROCESS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exit) {
			t.Fatalf("run check: %v; stderr: %s", err, &stderr)
		}
		return stdout.Bytes(), stderr.Bytes(), exit.ExitCode()
	}
	return stdout.Bytes(), stderr.Bytes(), 0
}

func TestCheckCLIProcess(t *testing.T) {
	if os.Getenv("PLATEN_TEST_CHECK_PROCESS") != "1" {
		t.Skip("subprocess helper")
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"platen"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing CLI arguments")
}
