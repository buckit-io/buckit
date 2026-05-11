# Buckit Release Process

## Cutting a Release

### Stable release

```sh
git tag RELEASE.2026-05-10T23-30-35Z
git push origin RELEASE.2026-05-10T23-30-35Z
```

Or generate the timestamp automatically:

```sh
TAG=RELEASE.$(date -u +%Y-%m-%dT%H-%M-%SZ)
git tag $TAG
git push origin $TAG
```

### Release candidate

```sh
TAG=RELEASE.$(date -u +%Y-%m-%dT%H-%M-%SZ).rc1
git tag $TAG
git push origin $TAG
```

### Tag format

The tag **must** follow `RELEASE.YYYY-MM-DDTHH-MM-SSZ` exactly. All
components (year, month, day, hour, minute, second) are required. The build
will fail if any part is missing.

Examples:
- `RELEASE.2026-05-10T23-30-35Z` — stable
- `RELEASE.2026-05-10T23-30-35Z.rc1` — release candidate

## What Happens Automatically

Once you push a tag, the **Release** workflow (`.github/workflows/release.yml`)
triggers. Monitor it at:

```
https://github.com/buckit-io/buckit/actions/workflows/release.yml
```

Or via CLI:

```sh
gh run list --workflow=release.yml --repo buckit-io/buckit
```

The workflow runs these jobs in order:

1. **build** — Compiles binaries for linux/amd64, linux/arm64, windows/amd64, darwin/arm64. Signs each with minisign. Generates `.deb`, `.rpm`, `.apk` packages (Linux only).
2. **docker** — Builds and pushes multi-arch Docker images to ghcr.io and Docker Hub.
3. **publish** — Creates a GitHub Release with all artifacts attached.
4. **update-gh-pages** — Updates the self-update pointer on GitHub Pages (stable releases only).

All jobs should show ✅. If any job fails, click into it to see the error log.

Release candidates skip the `:latest` Docker tag and the gh-pages update.

## Downloading Releases

### Binaries

```sh
# Linux
curl -LO https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-10T23-30-35Z/buckit-linux-amd64.RELEASE.2026-05-10T23-30-35Z

# macOS
curl -LO https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-10T23-30-35Z/buckit-darwin-arm64.RELEASE.2026-05-10T23-30-35Z

# Windows
curl -LO https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-10T23-30-35Z/buckit-windows-amd64.exe.RELEASE.2026-05-10T23-30-35Z
```

### Docker

```sh
docker pull ghcr.io/buckit-io/buckit:latest
docker pull buckitio/buckit:latest

# Or a specific version:
docker pull ghcr.io/buckit-io/buckit:RELEASE.2026-05-10T23-30-35Z
```

### Run with Docker

```sh
# Basic single-node server
docker run -p 9000:9000 -p 9001:9001 ghcr.io/buckit-io/buckit:latest server /data --console-address :9001

# With persistent storage
docker run -p 9000:9000 -p 9001:9001 -v ~/buckit-data:/data ghcr.io/buckit-io/buckit:latest server /data --console-address :9001

# With custom credentials
docker run -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=myadmin \
  -e MINIO_ROOT_PASSWORD=mysecretpassword \
  -v ~/buckit-data:/data \
  ghcr.io/buckit-io/buckit:latest server /data --console-address :9001

# Run in background
docker run -d --name buckit -p 9000:9000 -p 9001:9001 -v ~/buckit-data:/data ghcr.io/buckit-io/buckit:latest server /data --console-address :9001

# View logs
docker logs buckit

# Stop
docker stop buckit
```

Access the console at http://localhost:9001 and the S3 API at http://localhost:9000.
Default credentials: `buckitadmin` / `buckitadmin`.

### Linux Packages

DEB (Debian/Ubuntu):

```sh
wget https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-10T23-30-35Z/buckit_20260510233035.0.0_amd64.deb
dpkg -i buckit_20260510233035.0.0_amd64.deb
```

RPM (RHEL/Fedora):

```sh
dnf install https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-10T23-30-35Z/buckit-20260510233035.0.0-1.x86_64.rpm
```

APK (Alpine):

```sh
wget https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-10T23-30-35Z/buckit-20260510233035.0.0-r0.apk
apk add --allow-untrusted buckit-20260510233035.0.0-r0.apk
```

The packages install the binary to `/usr/local/bin/buckit` and include a
systemd service unit at `/lib/systemd/system/minio.service`.

## Verifying Signatures

Each binary has a `.minisig` signature file. To verify:

```sh
minisign -Vm buckit-linux-amd64.RELEASE.2026-05-10T23-30-35Z \
  -x buckit-linux-amd64.RELEASE.2026-05-10T23-30-35Z.minisig \
  -p buckit.pub
```

The public key (`buckit.pub`) is in the repository root.

## Upgrading a Running Server

### Using `mc admin update`

```sh
mc admin update <alias>
```

This automatically discovers the latest release and performs a rolling
update across all nodes in the cluster.

How it works: the server fetches a small `buckit.sha256sum` pointer file
from GitHub Pages to check if a newer version exists. One file per platform:

```
https://buckit-io.github.io/buckit/server/buckit/release/linux-amd64/buckit.sha256sum
https://buckit-io.github.io/buckit/server/buckit/release/linux-arm64/buckit.sha256sum
https://buckit-io.github.io/buckit/server/buckit/release/windows-amd64/buckit.sha256sum
https://buckit-io.github.io/buckit/server/buckit/release/darwin-arm64/buckit.sha256sum
```

Each file contains one line (`<sha256> buckit.RELEASE.<timestamp>`) which
the server uses to determine the latest version and verify the downloaded
binary. These files are updated automatically on each stable release (not
RCs) via the `gh-pages` branch.

You can also point to a specific release or a private mirror:

```sh
mc admin update <alias> https://my-mirror.example.com/buckit/linux-amd64/buckit.sha256sum
```

### Manual upgrade

Download the new binary, replace the old one, restart the service.

## Secrets

These are configured in GitHub Actions (Settings → Secrets):

| Secret | Purpose |
|--------|---------|
| `MINISIGN_PRIVATE_KEY` | Signs release binaries |
| `MINISIGN_PASSWORD` | Passphrase for the signing key |
| `DOCKERHUB_USERNAME` | Pushes images to Docker Hub |
| `DOCKERHUB_TOKEN` | Docker Hub access token |

`GITHUB_TOKEN` is provided automatically by GitHub Actions.

## Infrastructure

| Service | Purpose | URL |
|---------|---------|-----|
| GitHub Releases | Binary/package hosting | https://github.com/buckit-io/buckit/releases |
| GitHub Pages | Self-update pointer (`buckit.sha256sum`) | https://buckit-io.github.io/buckit/ |
| ghcr.io | Docker images | ghcr.io/buckit-io/buckit |
| Docker Hub | Docker images (mirror) | docker.io/buckitio/buckit |

## Workflow File

The release workflow lives at `.github/workflows/release.yml`.
