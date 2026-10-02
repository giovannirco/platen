package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/giovannirco/platen/internal/api"
	"github.com/giovannirco/platen/internal/mcpserver"
)

// cmdServe runs the web interface, the REST API and the MCP endpoint.
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file")
	debug := fs.Bool("debug", false, "log every request")
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

	mcpSrv := mcpserver.New(h, mcpserver.Options{Version: version, Stateless: true, Logger: log})
	handler := api.New(h, log, version, mcpserver.HTTPHandler(mcpSrv, log)).Handler()

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
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
	switch {
	case cfg.Auth.Open():
		log.Warn("no access token and no trusted network is set: anyone who can reach this address can print and scan (set auth.tokens or auth.trusted_networks)")
	case len(cfg.Auth.Tokens) == 0:
		log.Info("no access token is set: only clients in the trusted networks can use Platen")
	}

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
