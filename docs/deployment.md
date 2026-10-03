# Deployment and upgrades

Platen releases include static binaries for Linux and macOS on amd64/arm64, Windows on amd64, and `checksums.txt`. Container images are published as `ghcr.io/giovannirco/platen:<version>` for Linux amd64/arm64. Use an explicit version for a reproducible deployment; `latest` follows the newest release.

Keep your configuration, secret environment file and data directory outside the release archive or container. The data directory holds scans and print history. Preserve it across upgrades.

## Binary service

The following example upgrades a Linux amd64 installation using `platen.service`, `/usr/local/bin/platen`, `/etc/platen` and `/var/lib/platen`. Adjust the architecture, paths and service name to match your installation. Check that no print or scan is in progress before stopping the service.

Download and verify the release in a new directory:

```sh
mkdir platen-0.1.1
cd platen-0.1.1
curl --fail --location --remote-name https://github.com/giovannirco/platen/releases/download/v0.1.1/platen_0.1.1_linux_amd64.tar.gz
curl --fail --location --remote-name https://github.com/giovannirco/platen/releases/download/v0.1.1/checksums.txt
sha256sum --check --ignore-missing checksums.txt
tar -xzf platen_0.1.1_linux_amd64.tar.gz
./platen version
```

Only proceed if the checksum passes and the binary reports `platen 0.1.1`. Stop the service, save a private backup of configuration and data, then replace the binary while keeping the previous version:

```sh
sudo systemctl stop platen.service
sudo install -d -m 700 /var/backups/platen
sudo tar -czf /var/backups/platen/before-0.1.1.tar.gz -C / etc/platen var/lib/platen
sudo chmod 600 /var/backups/platen/before-0.1.1.tar.gz
sudo cp -p /usr/local/bin/platen /usr/local/bin/platen.previous
sudo install -m 755 platen /usr/local/bin/platen.next
sudo mv /usr/local/bin/platen.next /usr/local/bin/platen
sudo systemctl start platen.service
sudo systemctl is-active platen.service
```

The backup includes credentials and scanned documents; keep it private. Check `/healthz`, `/api/v1/info`, the device status and the web interface at your configured URL. Run `platen check -json -config /etc/platen/platen.yaml` with the service's secret environment loaded to verify device and Paperless connectivity. This command reports connectivity, not whether a printer is ready to accept a job.

If the upgrade fails, restore the saved executable and restart the service:

```sh
sudo cp -p /usr/local/bin/platen.previous /usr/local/bin/platen.next
sudo mv /usr/local/bin/platen.next /usr/local/bin/platen
sudo systemctl restart platen.service
```

For 0.1.0 → 0.1.1 no configuration or data migration is needed. Request new MCP approvals for print confirmations that were pending before the restart.

## Containers

For Compose, set `image: ghcr.io/giovannirco/platen:0.1.1` in your deployment and keep the existing data volume and configuration mount:

```sh
docker compose pull platen
docker compose up -d platen
```

For Kubernetes, update the image version in your deployment source and apply it through your usual deployment workflow. The [example](../deploy/kubernetes.yaml) uses a PVC and one replica with `Recreate` so the data volume moves between versions. Preserve the existing PVC and Secret. Review network access to the printer, scanner and Paperless for your cluster.

Keep the previous image version available for rollback and check the same health, device and browser endpoints after upgrading.
