// Package escl is a client for eSCL, the driverless scanning protocol behind
// Apple AirScan and Mopria Scan. It is plain HTTP and XML: read the scanner's
// capabilities, post a scan job, then fetch the pages.
package escl

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one scanner.
type Client struct {
	base string // e.g. http://scanner/eSCL
	http *http.Client
}

// Options tune a Client.
type Options struct {
	InsecureTLS bool
	HTTPClient  *http.Client
}

// New returns a client for the eSCL endpoint at baseURL (usually http://host/eSCL).
func New(baseURL string, opts Options) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("scanner url %q is not a URL", baseURL)
	}
	c := &Client{base: strings.TrimRight(baseURL, "/"), http: opts.HTTPClient}
	if c.http == nil {
		c.http = &http.Client{Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: opts.InsecureTLS}, //nolint:gosec // opt-in for self-signed scanner certificates
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: 5 * time.Minute, // a page at high resolution takes a while
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
		}}
	}
	return c, nil
}

// InputCaps describes one input (flatbed, feeder side).
type InputCaps struct {
	// Sizes are in 1/300 inch, the unit eSCL uses.
	MinWidth    int      `json:"min_width"`
	MaxWidth    int      `json:"max_width"`
	MinHeight   int      `json:"min_height"`
	MaxHeight   int      `json:"max_height"`
	ColorModes  []string `json:"color_modes"`
	Resolutions []int    `json:"resolutions"`
	Formats     []string `json:"formats"`
	Intents     []string `json:"intents,omitempty"`
}

// Capabilities is what a scanner reports about itself.
type Capabilities struct {
	Version      string     `json:"version"`
	MakeAndModel string     `json:"make_and_model"`
	SerialNumber string     `json:"serial_number,omitempty"`
	UUID         string     `json:"uuid,omitempty"`
	AdminURL     string     `json:"admin_url,omitempty"`
	Platen       *InputCaps `json:"platen,omitempty"`
	Feeder       *InputCaps `json:"feeder,omitempty"`
	FeederDuplex *InputCaps `json:"feeder_duplex,omitempty"`
}

type xmlInputCaps struct {
	MinWidth  int `xml:"MinWidth"`
	MaxWidth  int `xml:"MaxWidth"`
	MinHeight int `xml:"MinHeight"`
	MaxHeight int `xml:"MaxHeight"`
	Profiles  []struct {
		ColorModes  []string `xml:"ColorModes>ColorMode"`
		Formats     []string `xml:"DocumentFormats>DocumentFormat"`
		FormatsExt  []string `xml:"DocumentFormats>DocumentFormatExt"`
		Resolutions []struct {
			X int `xml:"XResolution"`
			Y int `xml:"YResolution"`
		} `xml:"SupportedResolutions>DiscreteResolutions>DiscreteResolution"`
		Range *struct {
			MinX int `xml:"XResolutionRange>Min"`
			MaxX int `xml:"XResolutionRange>Max"`
		} `xml:"SupportedResolutions>ResolutionRange"`
	} `xml:"SettingProfiles>SettingProfile"`
	Intents []string `xml:"SupportedIntents>Intent"`
}

type xmlCapabilities struct {
	XMLName      xml.Name `xml:"ScannerCapabilities"`
	Version      string   `xml:"Version"`
	MakeAndModel string   `xml:"MakeAndModel"`
	SerialNumber string   `xml:"SerialNumber"`
	UUID         string   `xml:"UUID"`
	AdminURI     string   `xml:"AdminURI"`
	Platen       *struct {
		Caps *xmlInputCaps `xml:"PlatenInputCaps"`
	} `xml:"Platen"`
	Adf *struct {
		Simplex *xmlInputCaps `xml:"AdfSimplexInputCaps"`
		Duplex  *xmlInputCaps `xml:"AdfDuplexInputCaps"`
	} `xml:"Adf"`
}

func (x *xmlInputCaps) convert() *InputCaps {
	if x == nil {
		return nil
	}
	c := &InputCaps{MinWidth: x.MinWidth, MaxWidth: x.MaxWidth, MinHeight: x.MinHeight, MaxHeight: x.MaxHeight, Intents: x.Intents}
	seen := map[string]bool{}
	add := func(list *[]string, kind, v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seen[kind+v] {
			seen[kind+v] = true
			*list = append(*list, v)
		}
	}
	res := map[int]bool{}
	for _, p := range x.Profiles {
		for _, m := range p.ColorModes {
			add(&c.ColorModes, "c", m)
		}
		for _, f := range append(p.Formats, p.FormatsExt...) {
			add(&c.Formats, "f", f)
		}
		for _, r := range p.Resolutions {
			if r.X == r.Y && r.X > 0 && !res[r.X] {
				res[r.X] = true
				c.Resolutions = append(c.Resolutions, r.X)
			}
		}
		if p.Range != nil && len(p.Resolutions) == 0 {
			for _, dpi := range []int{75, 100, 150, 200, 300, 400, 600, 1200} {
				if dpi >= p.Range.MinX && dpi <= p.Range.MaxX && !res[dpi] {
					res[dpi] = true
					c.Resolutions = append(c.Resolutions, dpi)
				}
			}
		}
	}
	return c
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s: HTTP %s", path, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// Capabilities reads what the scanner can do.
func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	raw, err := c.get(ctx, "/ScannerCapabilities")
	if err != nil {
		return nil, fmt.Errorf("scanner capabilities: %w", err)
	}
	return ParseCapabilities(raw)
}

// ParseCapabilities decodes a ScannerCapabilities document.
func ParseCapabilities(raw []byte) (*Capabilities, error) {
	var x xmlCapabilities
	if err := xml.Unmarshal(raw, &x); err != nil {
		return nil, fmt.Errorf("scanner capabilities: %w", err)
	}
	caps := &Capabilities{
		Version: x.Version, MakeAndModel: x.MakeAndModel, SerialNumber: x.SerialNumber,
		UUID: x.UUID, AdminURL: x.AdminURI,
	}
	if x.Platen != nil {
		caps.Platen = x.Platen.Caps.convert()
	}
	if x.Adf != nil {
		caps.Feeder = x.Adf.Simplex.convert()
		caps.FeederDuplex = x.Adf.Duplex.convert()
	}
	if caps.Platen == nil && caps.Feeder == nil {
		return nil, errors.New("scanner capabilities: no flatbed and no feeder reported")
	}
	return caps, nil
}

// Status is the scanner's current state.
type Status struct {
	// State is Idle, Processing, Testing, Stopped or Down.
	State string `json:"state"`
	// FeederState is set for scanners with a document feeder,
	// e.g. ScannerAdfLoaded or ScannerAdfEmpty.
	FeederState string `json:"feeder_state,omitempty"`
}

// Status reads the scanner's state.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	raw, err := c.get(ctx, "/ScannerStatus")
	if err != nil {
		return nil, fmt.Errorf("scanner status: %w", err)
	}
	var x struct {
		State    string `xml:"State"`
		AdfState string `xml:"AdfState"`
	}
	if err := xml.Unmarshal(raw, &x); err != nil {
		return nil, fmt.Errorf("scanner status: %w", err)
	}
	return &Status{State: x.State, FeederState: x.AdfState}, nil
}

// Settings describe a scan.
type Settings struct {
	// Source is "Platen" (flatbed) or "Feeder".
	Source string
	// Duplex scans both sides when the source is the feeder.
	Duplex bool
	// ColorMode is RGB24, Grayscale8 or BlackAndWhite1.
	ColorMode string
	// Resolution in dpi.
	Resolution int
	// Format is the MIME type to deliver: image/jpeg or application/pdf.
	Format string
	// Region in 1/300 inch. A zero Width or Height means the full area.
	X, Y, Width, Height int
	// Intent is a hint for the scanner's image processing: Document, Photo, TextAndGraphic.
	Intent string
}

func (s Settings) xml() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<scan:ScanSettings xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">` + "\n")
	b.WriteString("  <pwg:Version>2.0</pwg:Version>\n")
	if s.Intent != "" {
		fmt.Fprintf(&b, "  <scan:Intent>%s</scan:Intent>\n", s.Intent)
	}
	b.WriteString("  <pwg:ScanRegions pwg:MustHonor=\"true\">\n    <pwg:ScanRegion>\n")
	b.WriteString("      <pwg:ContentRegionUnits>escl:ThreeHundredthsOfInches</pwg:ContentRegionUnits>\n")
	fmt.Fprintf(&b, "      <pwg:XOffset>%d</pwg:XOffset>\n      <pwg:YOffset>%d</pwg:YOffset>\n", s.X, s.Y)
	fmt.Fprintf(&b, "      <pwg:Width>%d</pwg:Width>\n      <pwg:Height>%d</pwg:Height>\n", s.Width, s.Height)
	b.WriteString("    </pwg:ScanRegion>\n  </pwg:ScanRegions>\n")
	fmt.Fprintf(&b, "  <pwg:InputSource>%s</pwg:InputSource>\n", s.Source)
	if s.Source == "Feeder" {
		fmt.Fprintf(&b, "  <scan:Duplex>%t</scan:Duplex>\n", s.Duplex)
	}
	fmt.Fprintf(&b, "  <scan:ColorMode>%s</scan:ColorMode>\n", s.ColorMode)
	fmt.Fprintf(&b, "  <scan:XResolution>%d</scan:XResolution>\n  <scan:YResolution>%d</scan:YResolution>\n", s.Resolution, s.Resolution)
	fmt.Fprintf(&b, "  <pwg:DocumentFormat>%s</pwg:DocumentFormat>\n", s.Format)
	fmt.Fprintf(&b, "  <scan:DocumentFormatExt>%s</scan:DocumentFormatExt>\n", s.Format)
	b.WriteString("</scan:ScanSettings>\n")
	return b.Bytes()
}

// ErrBusy means the scanner is doing something else.
var ErrBusy = errors.New("the scanner is busy")

// Job is a scan in progress.
type Job struct {
	c   *Client
	url string
}

// Start begins a scan. Fetch the pages with NextDocument, then Close the job.
func (c *Client) Start(ctx context.Context, s Settings) (*Job, error) {
	if s.Source == "" {
		s.Source = "Platen"
	}
	if s.ColorMode == "" {
		s.ColorMode = "RGB24"
	}
	if s.Resolution <= 0 {
		s.Resolution = 300
	}
	if s.Format == "" {
		s.Format = "image/jpeg"
	}
	body := s.xml()
	// A scanner that just finished a job may answer 503 for a moment.
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/ScanJobs", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "text/xml")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("start scan: %w", err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusCreated:
			loc := resp.Header.Get("Location")
			if loc == "" {
				return nil, errors.New("start scan: the scanner returned no job location")
			}
			return &Job{c: c, url: c.rewrite(loc)}, nil
		case http.StatusServiceUnavailable:
			if attempt < 5 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2 * time.Second):
					continue
				}
			}
			return nil, ErrBusy
		case http.StatusConflict:
			return nil, fmt.Errorf("start scan: the scanner rejected the settings (HTTP %s)", resp.Status)
		default:
			return nil, fmt.Errorf("start scan: HTTP %s", resp.Status)
		}
	}
}

// rewrite keeps the path of the job location but uses the configured host.
// Scanners often answer with their mDNS name (xyz.local), which the server
// running Platen may not be able to resolve.
func (c *Client) rewrite(location string) string {
	base, err := url.Parse(c.base)
	if err != nil {
		return location
	}
	loc, err := url.Parse(location)
	if err != nil {
		return location
	}
	loc.Scheme, loc.Host = base.Scheme, base.Host
	return strings.TrimRight(loc.String(), "/")
}

// NextDocument returns the next scanned page. It returns io.EOF when the job has
// no more pages. The caller closes the body.
func (j *Job) NextDocument(ctx context.Context) (body io.ReadCloser, contentType string, err error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url+"/NextDocument", nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := j.c.http.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("fetch scanned page: %w", err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			return resp.Body, resp.Header.Get("Content-Type"), nil
		case http.StatusNotFound, http.StatusGone:
			resp.Body.Close()
			return nil, "", io.EOF
		case http.StatusServiceUnavailable:
			resp.Body.Close()
			if attempt < 30 {
				select {
				case <-ctx.Done():
					return nil, "", ctx.Err()
				case <-time.After(time.Second):
					continue
				}
			}
			return nil, "", ErrBusy
		default:
			resp.Body.Close()
			return nil, "", fmt.Errorf("fetch scanned page: HTTP %s", resp.Status)
		}
	}
}

// Close ends the job on the scanner. It is safe to call after all pages were read.
func (j *Job) Close(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, j.url, nil)
	if err != nil {
		return
	}
	if resp, err := j.c.http.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}
