package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"time"

	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giovannirco/platen/internal/api"
	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/mcpserver"
)

// Timeouts are sized for the default 100MB upload on a slow link, and for
// idle keep-alives between requests. WriteTimeout is left unset: the event
// stream is a long-lived response, and a write deadline would cut it off.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 5 * time.Minute
	idleTimeout       = 2 * time.Minute
)

// cmdServe runs the web interface, the REST API and the MCP endpoint.
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	debug := fs.Bool("debug", false, "log every request")
	insecureOpen := fs.Bool("insecure-open", false, "serve with no token and no trusted network on a non-loopback address (insecure; same gate as auth.open and PLATEN_ALLOW_OPEN=1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := logger(os.Stderr, *debug)
	h, err := newHub(*cfgPath, log)
	if err != nil {
		return err
	}
	defer h.Close()
	cfg := h.Config()
	if err := cfg.CheckOpenListen(*insecureOpen); err != nil {
		return err
	}

	mcpSrv := mcpserver.New(h, mcpserver.Options{Version: version, Stateless: true, Logger: log})
	handler := api.New(h, log, version, mcpserver.HTTPHandler(mcpSrv, log)).Handler()

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go h.RunMaintenance(ctx)
	h.ResumeJobs()

	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	log.Info("Platen is running", "version", version, "listen", ln.Addr().String(), "url", cfg.Server.BaseURL, "mcp", cfg.Server.BaseURL+"/mcp",
		"printers", len(cfg.Printers), "scanners", len(cfg.Scanners), "paperless", h.PaperlessEnabled(),
		"tokens", len(cfg.Auth.Tokens), "trusted_networks", cfg.Auth.TrustedNetworks)
	logOpenAuth(log, cfg)
	logInsecureTLS(log, cfg)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func logOpenAuth(log *slog.Logger, cfg *config.Config) {
	switch {
	case !cfg.Auth.Open():
		if len(cfg.Auth.Tokens) == 0 {
			log.Info("no access token is set: only clients in the trusted networks can use Platen")
		}
	case config.ListenIsLoopback(cfg.Server.Listen):
		log.Warn("no access token and no trusted network: Platen is open on a loopback address only")
	default:
		log.Warn("INSECURE: Platen is open on a non-loopback address because auth.open, --insecure-open or PLATEN_ALLOW_OPEN=1 was set. Anyone who can reach this address can print and scan")
	}
}

func logInsecureTLS(log *slog.Logger, cfg *config.Config) {
	var printers, scanners []string
	for _, p := range cfg.Printers {
		if p.InsecureTLS {
			printers = append(printers, p.ID)
		}
	}
	for _, s := range cfg.Scanners {
		if s.InsecureTLS {
			scanners = append(scanners, s.ID)
		}
	}
	if len(printers)+len(scanners) == 0 {
		return
	}
	log.Warn("TLS certificate verification is disabled for devices with insecure_tls; self-signed certificates are accepted",
		"printers", printers, "scanners", scanners)
	if os.Getenv("PLATEN_PRINTER_URI") != "" || os.Getenv("PLATEN_SCANNER_URL") != "" {
		log.Warn("PLATEN_PRINTER_URI and PLATEN_SCANNER_URL enable insecure TLS for the device they add")
	}
}

// cmdMCP runs the MCP server on standard input and output, for AI clients that
// start their tools as local programs.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Standard output carries the protocol, so logs go to standard error.
	log := logger(os.Stderr, false)
	h, err := newHub(*cfgPath, log)
	if err != nil {
		return err
	}
	defer h.Close()
	srv := mcpserver.New(h, mcpserver.Options{Version: version, Logger: log})
	err = srv.Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
