package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/giovannirco/platen/internal/hub"
)

// probeTimeout bounds a readiness or metrics device check so a silent printer
// cannot hold the probe open for the device client's full header timeout.
const probeTimeout = 8 * time.Second

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	result := s.probe(r.Context())
	status := http.StatusOK
	if !result.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": result.OK, "failed_checks": result.FailedChecks})
}

// metrics writes a small Prometheus text exposition. It is unauthenticated, like
// /healthz, and reports only up/down plus the version. Do not publish it on the
// public internet.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	result := s.probe(r.Context())
	var b strings.Builder
	b.WriteString("# HELP platen_up 1 if this process is serving.\n# TYPE platen_up gauge\nplaten_up 1\n")
	fmt.Fprintf(&b, "# HELP platen_info Build information.\n# TYPE platen_info gauge\nplaten_info{version=%s} 1\n", promQuote(s.version))
	b.WriteString("# HELP platen_device_up 1 if the configured device answered.\n# TYPE platen_device_up gauge\n")
	for _, p := range result.Printers {
		fmt.Fprintf(&b, "platen_device_up{kind=%s,id=%s} %d\n", promQuote("printer"), promQuote(p.ID), up(p.Online))
	}
	for _, sc := range result.Scanners {
		fmt.Fprintf(&b, "platen_device_up{kind=%s,id=%s} %d\n", promQuote("scanner"), promQuote(sc.ID), up(sc.Online))
	}
	if result.Paperless.Enabled {
		fmt.Fprintf(&b, "platen_device_up{kind=%s,id=%s} %d\n", promQuote("paperless"), promQuote("paperless"), up(result.Paperless.Online))
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) probe(ctx context.Context) *hub.CheckResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return s.hub.Check(ctx)
}

func up(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

func promQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
