# Changelog

## [0.1.2] — unreleased

Planned tag: `v0.1.2`. This entry is the release note for that tag. The tag is not created by the pull request that adds it.

### Security

- `platen serve` refuses to listen on a non-loopback address when no token and no trusted network are set. Loopback still runs for local development. The only opt-in is `auth.open: true`, `--insecure-open`, or `PLATEN_ALLOW_OPEN=1` (the same gate, logged as insecure).
- Compose publishes `127.0.0.1:8080:8080` and will not start without `PLATEN_AUTH_TOKENS` and `PLATEN_STATE_SECRET`.
- The web session cookie stores an HMAC of the bearer token, not the token. `X-Forwarded-Proto` is honoured only from `server.trusted_proxies`.
- HTTP read and idle timeouts are set. The read timeout covers a 100MB upload. The event stream is left without a write deadline.
- `SECURITY.md`, Dependabot for Go modules, GitHub Actions and Docker, `govulncheck` in CI, Actions pinned to commit SHAs, and a digest-pinned Dockerfile.
- The module requires Go 1.27.3 so builds pick up the standard-library fixes in that patch. `govulncheck` fails the job when it finds a reachable vulnerability.
- Release images get buildx provenance (`mode=max`), an SBOM and a keyless cosign signature. Binary releases keep `checksums.txt` and gain an SPDX SBOM plus a build-provenance attestation. `:latest` is not moved by a prerelease tag.

### Added

- `GET /readyz` reports whether configured printers, scanners and Paperless answered. `GET /metrics` exposes `platen_up`, `platen_info` and `platen_device_up` in Prometheus text. Both are unauthenticated, like `/healthz`.
- `platen healthcheck` probes the local process. Distroless images can use it as a health check.
- `platen version` after `go install` uses the module version from build info when the binary was not stamped with `-ldflags`.
- Deployment notes for the homelab binary-on-LXC path, including the Main VLAN reachability gap and the `nas-nvme` boot dependency.

### Changed

- MCP tool annotations set read-only, destructive, idempotent and open-world hints.
- Devices created from `PLATEN_PRINTER_URI` or `PLATEN_SCANNER_URL` log that insecure TLS is on.
- Docs state the supported device matrix honestly: Canon PIXMA G6010 / G6000 series for print, flatbed scan and ink; duplex and the document feeder are not verified on paper; URF is not implemented; Office documents are not converted.

### Upgrade notes

- An existing service that listens on every interface with an empty token list and no trusted network will not start until you set a token, a trusted network, a loopback bind, or an explicit insecure opt-in.
- Web sessions from 0.1.1 are cookies that contain the raw token. They stop working. Sign in again.
- Set `PLATEN_STATE_SECRET` (or `server.state_secret`) so a print confirmation and the web session survive a restart. Without it, a new random secret is used each process, and every restart signs the web UI out.
- No scan or history migration is required. Run one replica; the data directory is local.
- Container examples in this tree refer to `0.1.2`. That image exists only after the tag is pushed. Until then, the newest published release is `0.1.1`.

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
