package escl_test

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"io"
	"strings"
	"testing"

	"github.com/giovannirco/platen/internal/escl"
	"github.com/giovannirco/platen/internal/testutil"
)

func TestParseCapabilities(t *testing.T) {
	caps, err := escl.ParseCapabilities([]byte(testutil.Capabilities))
	if err != nil {
		t.Fatal(err)
	}
	if caps.MakeAndModel != "Platen Test Scanner" || caps.Version != "2.63" || caps.Feeder != nil || caps.Platen == nil {
		t.Fatalf("capabilities: %+v", caps)
	}
	p := caps.Platen
	if p.MaxWidth != 2550 || p.MaxHeight != 3508 || len(p.Resolutions) != 4 || len(p.ColorModes) != 2 || len(p.Formats) != 2 {
		t.Errorf("platen: %+v", p)
	}
	if _, err := escl.ParseCapabilities([]byte("<html>not a scanner</html>")); err == nil {
		t.Error("a web page is not a capabilities document")
	}
}

func TestScan(t *testing.T) {
	fake := testutil.NewScanner(t)
	ctx := context.Background()
	c, err := escl.New(fake.BaseURL(), escl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := c.Status(ctx); err != nil || st.State != "Idle" {
		t.Fatalf("status: %+v %v", st, err)
	}
	job, err := c.Start(ctx, escl.Settings{Resolution: 150, ColorMode: "Grayscale8", Width: 2480, Height: 3508})
	if err != nil {
		t.Fatal(err)
	}
	if req := fake.Requests[0]; !strings.Contains(req, `pwg:MustHonor="true"`) || !strings.Contains(req, "<pwg:InputSource>Platen</pwg:InputSource>") || !strings.Contains(req, "image/jpeg") {
		t.Errorf("scan settings:\n%s", req)
	}
	// The scanner answered with a host name that does not resolve; the client
	// must still find the page at the address it was configured with.
	body, contentType, err := job.NextDocument(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || contentType != "image/jpeg" || cfg.Width != 1240 || cfg.Height != 1754 {
		t.Errorf("page: %v %q %dx%d", err, contentType, cfg.Width, cfg.Height)
	}
	if _, _, err := job.NextDocument(ctx); !errors.Is(err, io.EOF) {
		t.Errorf("after the last page: %v", err)
	}
	job.Close(ctx)
}
