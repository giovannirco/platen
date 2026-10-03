package hub_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giovannirco/platen/internal/config"
	"github.com/giovannirco/platen/internal/hub"
)

func TestInlineTextRespectsDocumentLimit(t *testing.T) {
	f := newFixture(t, func(cfg *config.Config) { cfg.Limits.MaxUploadMB = 1 })
	_, err := f.hub.Print(context.Background(), hub.PrintRequest{
		Source: hub.Source{Text: strings.Repeat("x", 1<<20) + "é"}, DryRun: true,
	})
	if !errors.Is(err, hub.ErrLimitExceeded) || !strings.Contains(err.Error(), "document") {
		t.Fatalf("oversized text must fail the document limit before layout: %v", err)
	}
	if len(f.raster.Jobs()) != 0 {
		t.Fatal("oversized text reached the printer")
	}
}
