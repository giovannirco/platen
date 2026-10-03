package hub

import "context"

// CheckResult reports connectivity and capabilities for every configured device.
// OK means every configured service answered; it does not mean the printer is
// ready to print (its state and accepting_jobs fields report that separately).
type CheckResult struct {
	OK           bool            `json:"ok"`
	FailedChecks int             `json:"failed_checks"`
	Printers     []PrinterStatus `json:"printers"`
	Scanners     []ScannerStatus `json:"scanners"`
	Paperless    PaperlessCheck  `json:"paperless"`
}

// PaperlessCheck reports the optional archive's connectivity and metadata counts.
type PaperlessCheck struct {
	Enabled            bool   `json:"enabled"`
	Online             bool   `json:"online"`
	URL                string `json:"url,omitempty"`
	Error              string `json:"error,omitempty"`
	TagCount           int    `json:"tag_count"`
	CorrespondentCount int    `json:"correspondent_count"`
	DocumentTypeCount  int    `json:"document_type_count"`
}

// Check reads device status and Paperless metadata without printing, scanning or
// uploading documents. Unconfigured services do not count as failures.
func (h *Hub) Check(ctx context.Context) *CheckResult {
	result := &CheckResult{
		Printers: h.Printers(ctx),
		Scanners: h.Scanners(ctx),
		Paperless: PaperlessCheck{
			Enabled: h.PaperlessEnabled(),
		},
	}
	for _, p := range result.Printers {
		if !p.Online {
			result.FailedChecks++
		}
	}
	for _, s := range result.Scanners {
		if !s.Online {
			result.FailedChecks++
		}
	}
	if result.Paperless.Enabled {
		result.Paperless.URL = h.cfg.Paperless.PublicURL
		meta, err := h.PaperlessMeta(ctx)
		if err != nil {
			result.Paperless.Error = err.Error()
			result.FailedChecks++
		} else {
			result.Paperless.Online = true
			result.Paperless.TagCount = len(meta.Tags)
			result.Paperless.CorrespondentCount = len(meta.Correspondents)
			result.Paperless.DocumentTypeCount = len(meta.DocumentTypes)
		}
	}
	result.OK = result.FailedChecks == 0
	return result
}
