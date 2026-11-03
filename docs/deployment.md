# Deployment and upgrades

The supported production path is a **static binary and systemd on an LXC**. That is how the homelab runs Platen. Docker Compose and the Kubernetes manifest in [`deploy/`](../deploy/) are examples. They are not the production target until a cluster deployment has a real NetworkPolicy for the printer VLAN, an Ingress or Helm/Kustomize layout that someone actually applies, and alerting on `/readyz` or `/metrics`. The example manifest includes a NetworkPolicy sketch and the probes; it is still an example.

Releases include static binaries for Linux and macOS on amd64/arm64, Windows on amd64, and `checksums.txt`. Container images are `ghcr.io/giovannirco/platen:<version>` for Linux amd64/arm64. Pin that version. `:latest` moves when a stable release tag is pushed (a tag containing `-`, such as a prerelease, does not move it). Do not run production on `:latest`.

The image and binary for **0.1.2 exist only after the `v0.1.2` tag is pushed**. Until that tag exists, the newest published release is [v0.1.1](https://github.com/giovannirco/platen/releases/tag/v0.1.1). This document describes the 0.1.2 upgrade so the tag can be cut without a second doc change.

Keep configuration, the secret environment file and the data directory outside the release archive. The data directory holds scans and print history. Run **one** process against it. Preserve it across upgrades.

## Homelab: CT 146 `printhub`

Production is LXC container **146**, hostname `printhub`, on Proxmox host `cdd-prox-02` (Ren7000). It is not part of the Omni Kubernetes cluster.

Facts that are safe to write down here:

- The service is systemd, with `Restart=on-failure` and `MemoryMax=1G`.
- The binary is pinned to a release. The run notes record the checksum. Check it again on each upgrade (`sha256sum --check`).
- Configuration is maintained in the site's `tools/platen` files and installed on the container. Those files are not in this repository. Do not copy tokens into git.
- The token is taken from the password manager and installed as `/etc/platen/platen.env` mode `640`. The unit reads it with `EnvironmentFile=`.
- The process listens on port **8090** on this container. The binary running there today is **0.1.1**. On that version the meaningful HTTP probes are `/healthz` and `/api/v1/info` only. `/readyz` and `/metrics` arrive in **0.1.2**. Until the upgrade, those two paths are not probes: the 0.1.1 web interface answers them with **HTTP 200 and the SPA HTML**, so a blackbox check that treats any 200 as "up" is a false positive.

A unit that matches that shape:

```ini
[Service]
ExecStart=/usr/local/bin/platen serve -config /etc/platen/platen.yaml
EnvironmentFile=/etc/platen/platen.env
Restart=on-failure
MemoryMax=1G
NoNewPrivileges=true
```

`platen.env` should set `PLATEN_AUTH_TOKENS` and `PLATEN_STATE_SECRET` at least. Mode `640`, owner root, group the service account if the unit is not root.

### Main VLAN reachability (ops, not a code fix)

Checked around 3 November 2026:

- The Servers VLAN name for printhub, port 8090, answers `/healthz` and `/api/v1/info` and reports 0.1.1.
- The Main VLAN name and address on port 8090 do not accept TCP. That path worked on 3 October 2026.
- mDNS on Main still advertises "Canon G6010 @ printhub", so phones discover a printer path that does not connect.
- Family use without a token depends on the Main network being listed in `auth.trusted_networks`. That does nothing while Main cannot open the port.

This is an operations gap, not a release blocker. It needs a look at CT 146's interface, firewall and route, on the container and on the Proxmox host. Shipping v0.1.2 does not repair it. Until the path is fixed, phones on Main that follow the mDNS advertisement will fail, and a trusted-network rule for Main will not see those clients.

### Monitoring and boot

While CT 146 is still on **0.1.1**, probe `/healthz` (process up) and `/api/v1/info` (version and configuration). Do not point a blackbox check at `/readyz` or `/metrics` yet. On 0.1.1 those URLs fall through to the web interface and return **HTTP 200 with SPA HTML**, which looks healthy and is not.

`/healthz` only means the process is running. It does not mean the printer answered. **After the upgrade to 0.1.2**, use `/readyz` or `/metrics` (`platen_up`, `platen_device_up`) for device checks. A timer that runs `platen check -json` is valid on both versions. Alert when that command fails or when the container is down.

Two separate outages, not one duration:

- **22 October 2026:** a power cut left CT 146 down for about 24 hours with no alert. A blackbox probe of `/healthz` from another VLAN, or an alert when the CT is stopped, would have caught that. Device-down is a separate alert, and it belongs on `/readyz` or `platen_device_up` only once 0.1.2 is running.
- **2–3 November 2026:** about 11 hours from the host coming up around 21:12 until the containers were up around 08:01. See the boot note below. That gap is not the October silence.

**Boot and `nas-nvme`.** CT 146 (printhub), CT 147 (Paperless) and CT 140 (netboot) use an NFS root on `nas-nvme`. On the November outage the Proxmox host came up before that storage, so those containers failed to start for the ~11 hours above. Give them a start delay or an ordering dependency on `nas-nvme`, or move the root disk onto local storage. Confirm the nightly vzdump of CT 146 is still scheduled; this repository cannot see the backup job. Re-check it after a storage outage.

Operators who already have the private site10 notes can match this section to homelab-revamp PR #279 (`platen-02-ops-gaps`). That record is private. Nothing in this guide depends on opening it.

### Duplex

Two-sided printing is implemented. It has **not** been checked on paper on the Canon PIXMA G6010 (homelab follow-up 158). Do not treat a successful raster job as proof of duplex. The document feeder path is also untested on that hardware.

## Binary upgrade (0.1.1 → 0.1.2)

The example below upgrades a Linux amd64 install that uses `platen.service`, `/usr/local/bin/platen`, `/etc/platen` and `/var/lib/platen`. Adjust architecture, paths and the service name. On CT 146 the listen port is 8090 and the environment file is `/etc/platen/platen.env`. Stop the service only when no print or scan is in progress.

0.1.2 will not start if it binds a non-loopback address and both the token list and the trusted networks are empty. Set `PLATEN_AUTH_TOKENS` or `auth.trusted_networks` before restarting a service that previously ran open. Set `PLATEN_STATE_SECRET` so an in-flight print confirmation can survive the new process. Web browsers must sign in again: old session cookies held the raw token and are rejected.

Download and verify in a new directory, after the `v0.1.2` release exists:

```sh
mkdir platen-0.1.2
cd platen-0.1.2
curl --fail --location --remote-name https://github.com/giovannirco/platen/releases/download/v0.1.2/platen_0.1.2_linux_amd64.tar.gz
curl --fail --location --remote-name https://github.com/giovannirco/platen/releases/download/v0.1.2/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf platen_0.1.2_linux_amd64.tar.gz
./platen version
```

Only continue if the checksum matches and the binary prints `platen 0.1.2`. Stop the service, keep a private backup, and replace the binary:

```sh
sudo systemctl stop platen.service
sudo install -d -m 700 /var/backups/platen
sudo tar -czf /var/backups/platen/before-0.1.2.tar.gz -C / etc/platen var/lib/platen
sudo chmod 600 /var/backups/platen/before-0.1.2.tar.gz
sudo cp -p /usr/local/bin/platen /usr/local/bin/platen.previous
sudo install -m 755 platen /usr/local/bin/platen.next
sudo mv /usr/local/bin/platen.next /usr/local/bin/platen
sudo systemctl start platen.service
sudo systemctl is-active platen.service
```

The backup contains credentials and scanned documents. Keep it private. Then check `/healthz`, `/api/v1/info`, and the web UI. On 0.1.2, also check `/readyz` and `/metrics`. Do not treat `/readyz` or `/metrics` as probes until that version is the one listening: on 0.1.1 they return the SPA HTML with HTTP 200. `platen check -json -config /etc/platen/platen.yaml` with the service environment loaded reports device connectivity, not whether the printer is ready to accept a job. After 0.1.2, `platen_device_up` in `/metrics` is the same check for a scraper.

If the new process exits because authentication is missing, put the token back and start again. If it fails for another reason, restore the previous binary:

```sh
sudo cp -p /usr/local/bin/platen.previous /usr/local/bin/platen.next
sudo mv /usr/local/bin/platen.next /usr/local/bin/platen
sudo systemctl restart platen.service
```

No data migration is required from 0.1.1. Confirmations that were pending across the restart need `PLATEN_STATE_SECRET` to have been set on the old process as well; otherwise start them again.

## Containers

Compose and Kubernetes are examples. For Compose, pin `image: ghcr.io/giovannirco/platen:0.1.2` after that tag exists, keep the data volume, and copy [`deploy/.env.example`](../deploy/.env.example) to `deploy/.env`. The published port defaults to `127.0.0.1:8080`. To serve the LAN, replace that with one explicit host address. Do not switch the default to every interface.

```sh
docker compose pull platen
docker compose up -d platen
```

The container listens on every interface in its own network namespace, which is not loopback, so it refuses to start without a token. The health check runs `platen healthcheck` because the image has no shell.

The Kubernetes example uses one replica, a PVC, `/healthz` for liveness and `/readyz` for readiness. Preserve the PVC and the Secret, including `state-secret`. Pin the image digest from the release once it is published. The NetworkPolicy uses the documentation range `192.0.2.0/24` as a placeholder for the printer VLAN; replace it before applying.

## Image tags

| Tag | Meaning |
|---|---|
| `0.1.2` | Immutable release tag. Use this in production once it exists. |
| `0.1` | Moving minor tag. Avoid for the homelab pin. |
| `latest` | Newest stable release. Not updated by prerelease tags. Not for CT 146. |
