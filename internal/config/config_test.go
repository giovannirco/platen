package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platen.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	t.Setenv("PLATEN_PAPERLESS_TOKEN", "from-the-environment")
	cfg, err := Load(write(t, `
server:
  listen: ":9000"
printers:
  - id: office
    uri: ipp://printer.lan/ipp/print
  - id: photo
    name: Photo printer
    uri: ipps://10.0.0.9/ipp/print
    default: true
scanners:
  - id: flatbed
    url: http://printer.lan/eSCL/
paperless:
  url: http://paperless.lan:8000/
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.BaseURL != "http://localhost:9000" || cfg.Server.DataDir != "./data" {
		t.Errorf("server defaults: %+v", cfg.Server)
	}
	if cfg.DefaultPrinter().ID != "photo" || cfg.Printers[0].Name != "office" {
		t.Errorf("printers: %+v", cfg.Printers)
	}
	if cfg.DefaultScanner().URL != "http://printer.lan/eSCL" {
		t.Errorf("scanner url not normalised: %q", cfg.DefaultScanner().URL)
	}
	if !cfg.Paperless.Enabled() || cfg.Paperless.Token != "from-the-environment" || cfg.Paperless.PublicURL != "http://paperless.lan:8000" {
		t.Errorf("paperless: %+v", cfg.Paperless)
	}
	if cfg.Limits.ConfirmAboveSheets != 5 || cfg.Limits.MaxCopies != 10 || !cfg.Fetch.AllowURLs || cfg.Fetch.AllowPrivateNetworks {
		t.Errorf("defaults: %+v %+v", cfg.Limits, cfg.Fetch)
	}
}

func TestLoadRejectsMistakes(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown key":  {"server:\n  lisen: ':1'\n", "lisen"},
		"bad id":       {"printers:\n  - id: 'My Printer'\n    uri: ipp://x/ipp/print\n", "lower-case"},
		"bad scheme":   {"printers:\n  - id: p\n    uri: usb://x\n", "not supported"},
		"duplicate":    {"printers:\n  - id: p\n    uri: ipp://x/a\n  - id: p\n    uri: ipp://y/a\n", "duplicate"},
		"two defaults": {"printers:\n  - {id: a, uri: 'ipp://x/a', default: true}\n  - {id: b, uri: 'ipp://y/a', default: true}\n", "only one"},
		"scanner url":  {"scanners:\n  - id: s\n    url: escl://x\n", "http(s)"},
		"short token":  {"auth:\n  tokens: [short]\n", "16 characters"},
		"bad base url": {"server:\n  base_url: not-a-url\n", "absolute URL"},
		"bad network":  {"auth:\n  trusted_networks: ['192.168.1.0/33']\n", "auth.trusted_networks[0]"},
		"bad proxy":    {"server:\n  trusted_proxies: [the-proxy]\n", "server.trusted_proxies[0]"},
	} {
		_, err := Load(write(t, tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error mentioning %q", name, err, tc.want)
		}
	}
}

func TestEnvironmentOnly(t *testing.T) {
	t.Setenv("PLATEN_PRINTER_URI", "ipp://10.0.0.5/ipp/print")
	t.Setenv("PLATEN_SCANNER_URL", "http://10.0.0.5/eSCL")
	t.Setenv("PLATEN_AUTH_TOKENS", "first-token-0123456789, second-token-0123456789")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Printers) != 1 || !cfg.Printers[0].Default || len(cfg.Scanners) != 1 || len(cfg.Auth.Tokens) != 2 {
		t.Errorf("config from environment: %+v", cfg)
	}
}

func TestTrustedNetworks(t *testing.T) {
	t.Setenv("PLATEN_TRUSTED_PROXIES", "10.42.0.7, fd00:42::/64")
	cfg, err := Load(write(t, `
auth:
  trusted_networks: ["192.168.30.17/24", "10.42.30.5", "fd12:3456::/48"]
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range cfg.Auth.TrustedPrefixes {
		got = append(got, p.String())
	}
	// Networks are reduced to their base address; a bare address stands for one host.
	if want := "192.168.30.0/24 10.42.30.5/32 fd12:3456::/48"; strings.Join(got, " ") != want {
		t.Errorf("trusted networks: got %v, want %s", got, want)
	}
	if len(cfg.Server.ProxyPrefixes) != 2 || cfg.Server.ProxyPrefixes[0].String() != "10.42.0.7/32" {
		t.Errorf("trusted proxies: %v", cfg.Server.ProxyPrefixes)
	}
	if cfg.Auth.Open() {
		t.Error("a configuration with trusted networks is not open to everyone")
	}
	if !Default().Auth.Open() {
		t.Error("without tokens and trusted networks Platen is open")
	}
}
