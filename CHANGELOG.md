# Changelog

## [0.1.1](https://github.com/giovannirco/platen/releases/tag/v0.1.1) — 2026-10-03

### Added

- `platen check -json`: a structured connectivity report for printers, scanners and Paperless, with failure counts and exit status for scripts. It does not print, scan or upload a document.
- A deployment and upgrade guide, and versioned container images in the Compose and Kubernetes examples.

### Fixed

- Empty Paperless metadata and scan lists serialize as arrays. The web interface also tolerates older responses containing `null`, preventing the filing dialog from crashing.
- The web interface separates accepted print jobs from later history refresh failures and handles background refresh errors. A refresh failure no longer suggests submitting the same document again.
- A printer that refuses a job as busy produces HTTP 409 with error code `busy` and a message that the document was not accepted.
- JSON endpoints reject trailing values or malformed trailing content before starting work. Inline text observes the configured document size limit before rendering.
- Printing a scan prepares its PDF automatically, including after pages are added or edited.
- A complete PDF page selection in a different order, such as `2,1`, preserves that order through raster rendering, or is rejected before printing if the printer cannot accept that output.
- MCP print approvals bind the document bytes and resolved print settings. Changing the content or settings, including content fetched from the same URL, requires a new approval.

### Upgrade notes

- Existing configuration and stored scans remain compatible. See the [upgrade guide](docs/deployment.md).
- Restart pending MCP print confirmations after upgrading; 0.1.0 state does not carry the document fingerprint required by 0.1.1.
- The default development branch is now `master`.

## [0.1.0](https://github.com/giovannirco/platen/releases/tag/v0.1.0) — 2026-10-02

First release: IPP printing, in-process PDF rendering to PWG Raster, eSCL scan sessions, Paperless-ngx filing and search, a web interface, REST API and stateless HTTP/stdio MCP server with two MCP Apps views. Includes binaries for Linux and macOS on amd64/arm64, Windows on amd64, and a multi-architecture container image.
