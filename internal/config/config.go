// Package config loads and validates Platen's configuration.
//
// Configuration comes from a YAML file. Secrets and a few deployment values can be
// set or overridden by environment variables, so the same file can be kept in git.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the complete configuration.
type Config struct {
	Server    Server    `yaml:"server" json:"server"`
	Auth      Auth      `yaml:"auth" json:"-"`
	Printers  []Printer `yaml:"printers" json:"printers"`
	Scanners  []Scanner `yaml:"scanners" json:"scanners"`
	Paperless Paperless `yaml:"paperless" json:"paperless"`
	Limits    Limits    `yaml:"limits" json:"limits"`
	Fetch     Fetch     `yaml:"fetch" json:"fetch"`
}

// Server holds the HTTP listener and storage settings.
type Server struct {
	// Listen is the address the HTTP server binds to, e.g. ":8080".
	Listen string `yaml:"listen" json:"listen"`
	// BaseURL is the address people and agents use to reach Platen. It is used to
	// build links in API and MCP results. Defaults to http://localhost:<port>.
	BaseURL string `yaml:"base_url" json:"base_url"`
	// AllowedHosts lists extra host names Platen may be reached under. It only
	// matters when no access token is set: requests for any other name are then
	// refused, which stops DNS-rebinding attacks from a web page. IP addresses,
	// localhost, *.local names and the host of BaseURL are always accepted.
	// A single "*" turns the check off.
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts,omitempty"`
	// DataDir stores scans and job history.
	DataDir string `yaml:"data_dir" json:"data_dir"`
	// StateSecret signs the state that travels with multi-step MCP calls
	// (for example a print confirmation). Set it when running more than one replica;
	// otherwise a random one is generated at start.
	StateSecret string `yaml:"state_secret" json:"-"`
}

// Auth controls access to the API, the UI and the MCP endpoint.
type Auth struct {
	// Tokens are accepted bearer tokens. When empty, Platen is open to anyone who
	// can reach it, which is only suitable for a trusted network.
	Tokens []string `yaml:"tokens"`
}

// Printer is an IPP printer or print queue.
type Printer struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	// URI is the printer's IPP address: ipp://host/ipp/print for a printer,
	// ipp://host:631/printers/<queue> for a CUPS queue. ipps://, http:// and https:// also work.
	URI string `yaml:"uri" json:"uri"`
	// Default marks the printer used when a request names none.
	Default bool `yaml:"default" json:"default"`
	// InsecureTLS accepts self-signed certificates, which most printers have.
	InsecureTLS bool `yaml:"insecure_tls" json:"insecure_tls,omitempty"`
}

// Scanner is an eSCL (AirScan) scanner.
type Scanner struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	// URL is the eSCL base URL, usually http://host/eSCL.
	URL         string `yaml:"url" json:"url"`
	Default     bool   `yaml:"default" json:"default"`
	InsecureTLS bool   `yaml:"insecure_tls" json:"insecure_tls,omitempty"`
}

// Paperless is an optional Paperless-ngx instance to file documents into.
type Paperless struct {
	URL string `yaml:"url" json:"url"`
	// PublicURL is used for links shown to people when it differs from URL
	// (for example an internal service name versus the address in the browser).
	PublicURL string `yaml:"public_url" json:"public_url,omitempty"`
	Token     string `yaml:"token" json:"-"`
	// TokenFile is read at start when Token is empty.
	TokenFile string `yaml:"token_file" json:"-"`
}

// Enabled reports whether a Paperless instance is configured.
func (p Paperless) Enabled() bool { return p.URL != "" && p.Token != "" }

// Limits are guard rails. Printing uses paper and ink, so they matter most when
// the caller is an AI agent.
type Limits struct {
	// ConfirmAboveSheets asks for a confirmation when a job needs more sheets of
	// paper than this. 0 asks for every job, a negative value never asks.
	ConfirmAboveSheets int `yaml:"confirm_above_sheets" json:"confirm_above_sheets"`
	// MaxSheets rejects jobs above this many sheets. 0 means no limit.
	MaxSheets int `yaml:"max_sheets" json:"max_sheets"`
	// MaxCopies caps the copies of one job.
	MaxCopies int `yaml:"max_copies" json:"max_copies"`
	// MaxUploadMB caps the size of a document accepted for printing.
	MaxUploadMB int `yaml:"max_upload_mb" json:"max_upload_mb"`
	// ScanRetentionDays removes stored scans older than this. 0 keeps them forever.
	ScanRetentionDays int `yaml:"scan_retention_days" json:"scan_retention_days"`
}

// Fetch controls where Platen may read documents from when a request names a URL or a file.
type Fetch struct {
	// AllowURLs lets callers print a document by URL.
	AllowURLs bool `yaml:"allow_urls" json:"allow_urls"`
	// AllowPrivateNetworks also lets those URLs point at private, loopback and
	// link-local addresses. Off by default: with it on, anyone who can call Platen
	// can make it read from your internal network.
	AllowPrivateNetworks bool `yaml:"allow_private_networks" json:"allow_private_networks"`
	// AllowedDirs lists directories from which callers may print files by path.
	AllowedDirs []string `yaml:"allowed_dirs" json:"allowed_dirs"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Default returns a configuration with every default applied and no devices.
func Default() *Config {
	return &Config{
		Server: Server{Listen: ":8080", DataDir: "./data"},
		Limits: Limits{ConfirmAboveSheets: 5, MaxSheets: 100, MaxCopies: 10, MaxUploadMB: 100, ScanRetentionDays: 30},
		Fetch:  Fetch{AllowURLs: true},
	}
}

// Load reads the configuration file at path (if it is not empty), applies
// environment overrides and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() error {
	if v := os.Getenv("PLATEN_LISTEN"); v != "" {
		c.Server.Listen = v
	}
	if v := os.Getenv("PLATEN_BASE_URL"); v != "" {
		c.Server.BaseURL = v
	}
	if v := os.Getenv("PLATEN_ALLOWED_HOSTS"); v != "" {
		c.Server.AllowedHosts = splitList(v)
	}
	if v := os.Getenv("PLATEN_DATA_DIR"); v != "" {
		c.Server.DataDir = v
	}
	if v := os.Getenv("PLATEN_STATE_SECRET"); v != "" {
		c.Server.StateSecret = v
	}
	if v := os.Getenv("PLATEN_AUTH_TOKENS"); v != "" {
		c.Auth.Tokens = splitList(v)
	}
	if v := os.Getenv("PLATEN_PAPERLESS_URL"); v != "" {
		c.Paperless.URL = v
	}
	if v := os.Getenv("PLATEN_PAPERLESS_TOKEN"); v != "" {
		c.Paperless.Token = v
	}
	// A single printer and scanner can be configured without a file, which is
	// handy for a first run in a container.
	if v := os.Getenv("PLATEN_PRINTER_URI"); v != "" && len(c.Printers) == 0 {
		c.Printers = []Printer{{ID: "printer", Name: "Printer", URI: v, Default: true, InsecureTLS: true}}
	}
	if v := os.Getenv("PLATEN_SCANNER_URL"); v != "" && len(c.Scanners) == 0 {
		c.Scanners = []Scanner{{ID: "scanner", Name: "Scanner", URL: v, Default: true, InsecureTLS: true}}
	}
	if c.Paperless.Token == "" && c.Paperless.TokenFile != "" {
		raw, err := os.ReadFile(c.Paperless.TokenFile)
		if err != nil {
			return fmt.Errorf("read paperless token file: %w", err)
		}
		c.Paperless.Token = strings.TrimSpace(string(raw))
	}
	return nil
}

// Validate checks the configuration and fills derived defaults.
func (c *Config) Validate() error {
	var errs []error
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = "./data"
	}
	if c.Server.BaseURL == "" {
		port := c.Server.Listen
		if i := strings.LastIndex(port, ":"); i >= 0 {
			port = port[i+1:]
		}
		c.Server.BaseURL = "http://localhost:" + port
	}
	c.Server.BaseURL = strings.TrimRight(c.Server.BaseURL, "/")
	if u, err := url.Parse(c.Server.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("server.base_url %q is not an absolute URL", c.Server.BaseURL))
	}

	seen := map[string]bool{}
	defaults := 0
	for i := range c.Printers {
		p := &c.Printers[i]
		if !idPattern.MatchString(p.ID) {
			errs = append(errs, fmt.Errorf("printers[%d].id %q: use lower-case letters, digits, '-' or '_'", i, p.ID))
		}
		if seen["p:"+p.ID] {
			errs = append(errs, fmt.Errorf("printers: duplicate id %q", p.ID))
		}
		seen["p:"+p.ID] = true
		if p.Name == "" {
			p.Name = p.ID
		}
		u, err := url.Parse(p.URI)
		if err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("printers[%d].uri %q is not a URL", i, p.URI))
		} else {
			switch u.Scheme {
			case "ipp", "ipps", "http", "https":
			default:
				errs = append(errs, fmt.Errorf("printers[%d].uri: scheme %q is not supported (use ipp, ipps, http or https)", i, u.Scheme))
			}
		}
		if p.Default {
			defaults++
		}
	}
	if defaults > 1 {
		errs = append(errs, errors.New("printers: only one printer can be the default"))
	}
	if defaults == 0 && len(c.Printers) > 0 {
		c.Printers[0].Default = true
	}

	defaults = 0
	for i := range c.Scanners {
		s := &c.Scanners[i]
		if !idPattern.MatchString(s.ID) {
			errs = append(errs, fmt.Errorf("scanners[%d].id %q: use lower-case letters, digits, '-' or '_'", i, s.ID))
		}
		if seen["s:"+s.ID] {
			errs = append(errs, fmt.Errorf("scanners: duplicate id %q", s.ID))
		}
		seen["s:"+s.ID] = true
		if s.Name == "" {
			s.Name = s.ID
		}
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("scanners[%d].url %q must be an http(s) URL such as http://host/eSCL", i, s.URL))
		}
		s.URL = strings.TrimRight(s.URL, "/")
		if s.Default {
			defaults++
		}
	}
	if defaults > 1 {
		errs = append(errs, errors.New("scanners: only one scanner can be the default"))
	}
	if defaults == 0 && len(c.Scanners) > 0 {
		c.Scanners[0].Default = true
	}

	c.Paperless.URL = strings.TrimRight(c.Paperless.URL, "/")
	c.Paperless.PublicURL = strings.TrimRight(c.Paperless.PublicURL, "/")
	if c.Paperless.URL != "" {
		if u, err := url.Parse(c.Paperless.URL); err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("paperless.url %q is not a URL", c.Paperless.URL))
		}
		if c.Paperless.PublicURL == "" {
			c.Paperless.PublicURL = c.Paperless.URL
		}
	}

	if c.Limits.MaxCopies <= 0 {
		c.Limits.MaxCopies = 10
	}
	if c.Limits.MaxUploadMB <= 0 {
		c.Limits.MaxUploadMB = 100
	}
	for i, t := range c.Auth.Tokens {
		if len(strings.TrimSpace(t)) < 16 {
			errs = append(errs, fmt.Errorf("auth.tokens[%d] is shorter than 16 characters", i))
		}
	}
	return errors.Join(errs...)
}

// DefaultPrinter returns the default printer, or nil when none is configured.
func (c *Config) DefaultPrinter() *Printer {
	for i := range c.Printers {
		if c.Printers[i].Default {
			return &c.Printers[i]
		}
	}
	return nil
}

// DefaultScanner returns the default scanner, or nil when none is configured.
func (c *Config) DefaultScanner() *Scanner {
	for i := range c.Scanners {
		if c.Scanners[i].Default {
			return &c.Scanners[i]
		}
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
