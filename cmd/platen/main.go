// Command platen is a print and scan hub for people, programs and AI agents.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/hub"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Platen: your printer and scanner for people, programs and AI agents.

Usage:
  platen serve   [flags]          run the web interface, the REST API and the MCP endpoint
  platen mcp     [flags]          run the MCP server on standard input/output (for local AI clients)
  platen check   [flags]          talk to every configured device and report what it can do
  platen print   [flags] <file>   print a file ("-" reads standard input)
  platen scan    [flags]          scan a document to a file
  platen version

Common flags:
  -config <file>   configuration file (default: $PLATEN_CONFIG, then ./platen.yaml if it exists)

Run "platen <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = cmdServe(ctx, args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "check":
		err = cmdCheck(ctx, args)
	case "print":
		err = cmdPrint(ctx, args)
	case "scan":
		err = cmdScan(ctx, args)
	case "version", "-v", "--version":
		fmt.Println("platen", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "platen: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "platen:", err)
		os.Exit(1)
	}
}

// loadConfig finds and loads the configuration.
func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		path = os.Getenv("PLATEN_CONFIG")
	}
	if path == "" {
		if _, err := os.Stat("platen.yaml"); err == nil {
			path = "platen.yaml"
		}
	}
	return config.Load(path)
}

func newHub(configPath string, log *slog.Logger) (*hub.Hub, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	return hub.New(cfg, log)
}

func logger(w io.Writer, debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	jsonOutput := fs.Bool("json", false, "report connectivity and capabilities as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := newHub(*cfgPath, logger(os.Stderr, false))
	if err != nil {
		return err
	}
	defer h.Close()
	result := h.Check(ctx)
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return fmt.Errorf("write check report: %w", err)
		}
	} else {
		printCheckText(result)
	}
	if result.FailedChecks > 0 {
		return fmt.Errorf("%d check(s) failed", result.FailedChecks)
	}
	return nil
}

func printCheckText(result *hub.CheckResult) {
	printers := result.Printers
	if len(printers) == 0 {
		fmt.Println("No printers configured.")
	}
	for _, p := range printers {
		if !p.Online {
			fmt.Printf("✗ printer %s (%s): %s\n", p.ID, p.Name, p.Error)
			continue
		}
		fmt.Printf("✓ printer %s (%s): %s, %s\n", p.ID, p.Name, p.MakeAndModel, p.State)
		fmt.Printf("    accepts:  %s\n", strings.Join(p.Formats, ", "))
		if len(p.RasterDPI) > 0 {
			fmt.Printf("    raster:   %v dpi, %s\n", p.RasterDPI, strings.Join(p.RasterTypes, ", "))
		}
		fmt.Printf("    paper:    loaded %s; duplex %t; colour %t\n", orNone(strings.Join(p.MediaLabels, ", ")),
			len(p.Sides) > 1, containsString(p.ColorModes, "color"))
		for _, m := range p.Markers {
			level := "unknown"
			if m.Level >= 0 {
				level = fmt.Sprintf("%d%%", m.Level)
			}
			fmt.Printf("    ink:      %-14s %s\n", m.Name, level)
		}
		if !p.Supports("application/pdf") {
			if p.Supports("image/pwg-raster") {
				fmt.Println("    note:     this printer does not accept PDF; Platen renders PDFs itself and sends raster.")
			} else {
				fmt.Println("    warning:  this printer accepts neither PDF nor PWG raster; only pictures can be printed.")
			}
		}
	}

	scanners := result.Scanners
	if len(scanners) == 0 {
		fmt.Println("No scanners configured.")
	}
	for _, s := range scanners {
		if !s.Online {
			fmt.Printf("✗ scanner %s (%s): %s\n", s.ID, s.Name, s.Error)
			continue
		}
		fmt.Printf("✓ scanner %s (%s): %s, %s\n", s.ID, s.Name, s.MakeAndModel, s.State)
		fmt.Printf("    sources:  %s; up to %.0f x %.0f mm\n", strings.Join(s.Sources, ", "), s.MaxWidthMM, s.MaxHeightMM)
		fmt.Printf("    modes:    %s; %v dpi\n", strings.Join(s.ColorModes, ", "), s.Resolutions)
	}

	if p := result.Paperless; p.Enabled {
		if !p.Online {
			fmt.Printf("✗ paperless: %s\n", p.Error)
		} else {
			fmt.Printf("✓ paperless at %s: %d tag(s), %d correspondent(s), %d document type(s)\n",
				p.URL, p.TagCount, p.CorrespondentCount, p.DocumentTypeCount)
		}
	} else {
		fmt.Println("Paperless-ngx is not configured.")
	}
}

func cmdPrint(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("print", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	printer := fs.String("printer", "", "printer id (default: the default printer)")
	copies := fs.Int("copies", 1, "number of copies")
	duplex := fs.String("duplex", "off", "off, long-edge or short-edge")
	color := fs.String("color", "auto", "auto, color or monochrome")
	quality := fs.String("quality", "normal", "draft, normal or high")
	media := fs.String("media", "", "paper size: a4, letter, ... (default: what the printer has loaded)")
	pages := fs.String("pages", "", `pages to print, e.g. "1-3,5"`)
	title := fs.String("title", "", "job title")
	dryRun := fs.Bool("dry-run", false, "report pages and sheets without printing")
	yes := fs.Bool("yes", false, "print even when the job is above the confirmation threshold")
	dump := fs.String("dump", "", "write what would be sent to the printer to this file instead of printing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("print: give exactly one file (or - for standard input)")
	}
	var data []byte
	var err error
	name := fs.Arg(0)
	if name == "-" {
		data, err = io.ReadAll(os.Stdin)
		name = "standard input"
	} else {
		data, err = os.ReadFile(name)
		name = filepath.Base(name)
	}
	if err != nil {
		return err
	}
	h, err := newHub(*cfgPath, logger(os.Stderr, false))
	if err != nil {
		return err
	}
	defer h.Close()
	req := hub.PrintRequest{
		Printer: *printer, Source: hub.Source{Data: data, Name: name}, Title: *title, Copies: *copies,
		Duplex: *duplex, Color: *color, Quality: *quality, Media: *media, Pages: *pages,
		DryRun: *dryRun, Confirm: *yes, Via: "cli",
	}
	if *dump != "" {
		f, err := os.Create(*dump)
		if err != nil {
			return err
		}
		defer f.Close()
		req.Dump = f
	}
	res, err := h.Print(ctx, req)
	var need *hub.ConfirmationRequired
	if errors.As(err, &need) {
		return fmt.Errorf("%v (run again with -yes)", err)
	}
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	return nil
}

func cmdScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	scanner := fs.String("scanner", "", "scanner id (default: the default scanner)")
	out := fs.String("out", "", "output file (.pdf or .jpg; default: scan-<time>.pdf)")
	color := fs.String("color", "color", "color or gray")
	dpi := fs.Int("dpi", 300, "resolution")
	paper := fs.String("paper", "a4", "a4, letter, legal, a5, full or WxH in millimetres")
	source := fs.String("source", "", "flatbed or feeder (default: flatbed when there is one)")
	pagesFlag := fs.Int("pages", 1, "number of flatbed pages; Platen waits for Enter between pages")
	file := fs.Bool("paperless", false, "also file the document in Paperless-ngx")
	title := fs.String("title", "", "document title")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := newHub(*cfgPath, logger(os.Stderr, false))
	if err != nil {
		return err
	}
	defer h.Close()
	start := time.Now()
	sc, err := h.StartScan(ctx, hub.ScanRequest{Scanner: *scanner, Source: *source, Color: *color, Resolution: *dpi, Paper: *paper, Title: *title, Via: "cli"})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "page %d scanned (%s)\n", len(sc.Pages), time.Since(start).Round(100*time.Millisecond))
	for sc.Source == "flatbed" && len(sc.Pages) < *pagesFlag {
		fmt.Fprintf(os.Stderr, "Put page %d on the glass and press Enter... ", len(sc.Pages)+1)
		if _, err := fmt.Scanln(); err != nil && err.Error() != "unexpected newline" {
			return err
		}
		if sc, err = h.ScanNextPage(ctx, sc.ID); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "page %d scanned\n", len(sc.Pages))
	}
	format := "pdf"
	if ext := strings.ToLower(filepath.Ext(*out)); ext == ".jpg" || ext == ".jpeg" {
		format = "jpeg"
	}
	if sc, err = h.FinishScan(sc.ID, format); err != nil {
		return err
	}
	target := *out
	if target == "" {
		target = hub.ScanFilename(sc)
	}
	data, err := os.ReadFile(sc.Document.File)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d page(s), %d KB\n", target, len(sc.Pages), len(data)>>10)
	if *file {
		sc, err = h.FileScan(ctx, sc.ID, hub.FileRequest{Title: *title, Wait: true})
		if err != nil {
			return err
		}
		fmt.Printf("Paperless: %s %s\n", sc.Paperless.Status, sc.Paperless.URL)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
