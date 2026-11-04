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
	if !Default().Auth.Open() || Default().Auth.AllowOpen {
		t.Error("without tokens and trusted networks Platen is open, and that is not an explicit opt-in")
	}
}

func TestListenIsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.2:1", "localhost:8080", "[::1]:8080", "::1"} {
		if !ListenIsLoopback(addr) {
			t.Errorf("%s should be loopback", addr)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.0.2.10:8080", "printer.lan:8080"} {
		if ListenIsLoopback(addr) {
			t.Errorf("%s should not be loopback", addr)
		}
	}
}

func TestCheckOpenListen(t *testing.T) {
	open := func(listen string) *Config {
		cfg := Default()
		cfg.Server.Listen = listen
		return cfg
	}
	if err := open(":8080").CheckOpenListen(false); err == nil || !strings.Contains(err.Error(), "refusing to listen") {
		t.Fatalf("non-loopback open auth: %v", err)
	}
	if err := open("0.0.0.0:8080").CheckOpenListen(false); err == nil {
		t.Fatal("0.0.0.0 without opt-in should be refused")
	}
	if err := open("127.0.0.1:8080").CheckOpenListen(false); err != nil {
		t.Fatal(err)
	}
	if err := open("[::1]:8080").CheckOpenListen(false); err != nil {
		t.Fatal(err)
	}
	if err := open("localhost:8090").CheckOpenListen(false); err != nil {
		t.Fatal(err)
	}
	if err := open(":8080").CheckOpenListen(true); err != nil {
		t.Fatalf("--insecure-open: %v", err)
	}
	cfg := open("0.0.0.0:8080")
	cfg.Auth.AllowOpen = true
	if err := cfg.CheckOpenListen(false); err != nil {
		t.Fatalf("auth.open: %v", err)
	}
	cfg = open(":8080")
	cfg.Auth.Tokens = []string{"0123456789abcdef"}
	if err := cfg.CheckOpenListen(false); err != nil {
		t.Fatalf("token set: %v", err)
	}
	cfg = open(":8080")
	cfg.Auth.TrustedNetworks = []string{"192.0.2.0/24"}
	if err := cfg.CheckOpenListen(false); err != nil {
		t.Fatalf("trusted network: %v", err)
	}
}

func TestAllowOpenEnvIsTheSameGate(t *testing.T) {
	t.Setenv("PLATEN_LISTEN", "0.0.0.0:8080")
	t.Setenv("PLATEN_ALLOW_OPEN", "1")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Auth.AllowOpen || !cfg.Auth.Open() {
		t.Fatalf("PLATEN_ALLOW_OPEN=1 should set the explicit opt-in: %+v", cfg.Auth)
	}
	if err := cfg.CheckOpenListen(false); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PLATEN_ALLOW_OPEN", "false")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.AllowOpen {
		t.Fatal("PLATEN_ALLOW_OPEN=false must not enable open mode")
	}
	if err := cfg.CheckOpenListen(false); err == nil {
		t.Fatal("a negative PLATEN_ALLOW_OPEN must not bypass the refuse")
	}
}
