# Security policy

Platen can print, scan and read documents. Treat a reachable Platen as a way to use the printer and anything it is allowed to fetch.

## Reporting a vulnerability

Please report vulnerabilities privately. Use [GitHub private vulnerability reporting](https://github.com/giovannirco/platen/security/advisories/new) for this repository.

Do not open a public GitHub issue for a security report, and do not include tokens, scanned documents or live network addresses in the report.

A repository admin still has to enable private vulnerability reporting (Settings → Advanced Security → Private vulnerability reporting). The token used to prepare the v0.1.2 changes could not change that setting (`administration` permission is required). Until it is enabled, contact the maintainer directly and avoid a public issue.

The same limitation applies to Dependabot security updates. Dependabot version updates are configured in [`.github/dependabot.yml`](.github/dependabot.yml) for Go modules, GitHub Actions and Docker. An admin should also enable Dependabot alerts and Dependabot security updates on the repository.

## Supported versions

Security fixes go into the next release on `master`. The planned tag for the changes that introduced this policy is `v0.1.2`. Older 0.1.x tags are early builds; upgrade rather than expecting backports.

## Defaults

Platen does not start an open service on a non-loopback address.

- With no `auth.tokens` and no `auth.trusted_networks`, `platen serve` listens only on a loopback address (`127.0.0.1`, `::1`, `localhost`).
- `:8080`, `0.0.0.0` and any other non-loopback address are refused in that configuration.
- The same refusal is lifted only by an explicit opt-in: `auth.open: true`, `--insecure-open`, or `PLATEN_ALLOW_OPEN=1`. Those three are the same gate. A warning is written at startup. Setting the variable to `0` or `false` does not enable it and does not turn a yaml opt-in off.
- `deploy/compose.yaml` publishes `127.0.0.1:8080:8080` and refuses to start unless `PLATEN_AUTH_TOKENS` and `PLATEN_STATE_SECRET` are set. Inside the container the process listens on every interface so Docker can publish the port; that is why the token is mandatory even when the host port is loopback-only. To reach Platen from other machines, publish one explicit host address and keep the token.

Generate a token with `openssl rand -hex 24` (at least 16 characters).

## What a token can do

One bearer token is one trust domain. It can print, scan, cancel, file documents and call every MCP tool. There are no per-tool scopes. Give agents and scripts their own token only if you are willing to rotate that whole token; it is not a limited role.

`auth.trusted_networks` lets those clients in with no token. A trusted network that is the whole LAN is the same as sharing the printer with everyone on that LAN. Name a reverse proxy in `server.trusted_proxies` or every client behind it looks like the proxy.

The web session cookie is an HMAC of the token, not the token itself. A stolen cookie can be replayed as a cookie until it expires; it is not a bearer token. `X-Forwarded-Proto` is honoured only from a trusted proxy, and only to mark the cookie `Secure`.

`/healthz`, `/readyz` and `/metrics` are unauthenticated so a local probe can run. They do not return tokens. `/readyz` and `/metrics` do show whether configured devices answered. Do not publish them on the public internet. `/healthz` only means the process is up.

## Paperless

Use a Paperless-ngx user that can upload and search the documents this hub should see, not an admin account. Pass the token in `PLATEN_PAPERLESS_TOKEN` or `paperless.token_file`. Do not commit it.

## Supply chain

Continuous integration runs `govulncheck` and fails on findings. Release images are pushed with build provenance (`mode=max`) and an SBOM, and signed with keyless cosign. GitHub Actions are pinned to commit SHAs. The Go build image and the distroless base image are pinned by digest. Binary releases still include `checksums.txt`.

Production installs should pin a version tag (and the checksum), not the moving `:latest` tag. `:latest` is updated only for release tags that do not contain a hyphen, so a prerelease does not move it. A GitHub release whose tag contains `-` is published with `--prerelease` and `--latest=false`, so the GitHub Latest marker does not move either.

Verify a published image with the keyless certificate from the release workflow:

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/giovannirco/platen/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/giovannirco/platen@sha256:<digest>
```

## Out of scope for this policy

Platen does not sandbox the printer. Anyone who can call it can use paper and ink, up to the configured sheet and copy limits. URL fetches do not follow private addresses unless `fetch.allow_private_networks` is set. Printing by filesystem path stays off until `fetch.allowed_dirs` lists a directory.
