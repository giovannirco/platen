package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"
)

// cmdHealthcheck probes the local server. Distroless images have no shell or
// curl, so Compose and other supervisors can run this binary as the check.
func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "URL to probe (default: http://127.0.0.1:<port>/healthz from PLATEN_LISTEN)")
	ready := fs.Bool("ready", false, "probe /readyz instead of /healthz")
	timeout := fs.Duration("timeout", 3*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := *urlFlag
	if url == "" {
		path := "/healthz"
		if *ready {
			path = "/readyz"
		}
		url = localProbeURL(os.Getenv("PLATEN_LISTEN"), path)
	}
	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck %s: HTTP %d", url, resp.StatusCode)
	}
	return nil
}

// localProbeURL is the loopback URL for a listen address. Binding 0.0.0.0 or
// every interface still accepts connections on 127.0.0.1.
func localProbeURL(listen, path string) string {
	host := "127.0.0.1"
	port := "8080"
	if h, p, err := net.SplitHostPort(listen); err == nil && p != "" {
		port = p
		if ip, err := netip.ParseAddr(h); err == nil && ip.Unmap().IsLoopback() {
			if ip.Is4() {
				host = ip.Unmap().String()
			} else {
				host = "[" + ip.Unmap().String() + "]"
			}
		}
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + host + ":" + port + path
}
