# Buckit Release Process Design

## Overview

This document describes the automated release process for Buckit, producing multi-arch binaries, Linux packages, and Docker images via a GitHub Actions workflow triggered by tag push.

## Artifacts Produced

| Artifact | Format | Location |
|----------|--------|----------|
| Binaries | `buckit-{arch}.RELEASE.xxx` | GitHub Release |
| Checksums | `.sha256sum` | GitHub Release |
| Signatures | `.minisig` | GitHub Release |
| Debian package | `.deb` | GitHub Release |
| RPM package | `.rpm` | GitHub Release |
| Alpine package | `.apk` | GitHub Release |
| Docker image | multi-arch | `ghcr.io/buckit-io/buckit:{tag}` + `:latest` |
| Docker image | multi-arch | `docker.io/buckitio/buckit:{tag}` + `:latest` |

## Architecture

```
Tag Push (RELEASE.*)
        │
        ▼
┌─── Build (matrix: amd64, arm64) ───┐
│  • Compile binary (CGO_ENABLED=0)   │
│  • SHA-256 checksum                 │
│  • Minisign signature               │
│  • .deb / .rpm / .apk via pkger     │
└──────────────┬──────────────────────┘
               │
       ┌───────┴───────┐
       ▼               ▼
   Docker           Publish
   • buildx         • GitHub Release
   • multi-arch     • Attach all artifacts
   • push ghcr.io   • Auto release notes
   • push Docker Hub
```

### Job 1: Build (parallelized per architecture)

- Compiles the Go binary with release ldflags
- Generates SHA-256 checksum
- Signs with minisign
- Runs `pkger` to produce `.deb`, `.rpm`, `.apk`
- Uploads all outputs as workflow artifacts

### Job 2: Docker (depends on Build)

- Downloads binaries for both architectures
- Uses `docker/setup-buildx-action` + QEMU for cross-platform builds
- Builds multi-arch image (`linux/amd64`, `linux/arm64`) using the root `Dockerfile`
- Pushes to `ghcr.io/buckit-io/buckit` and `docker.io/buckitio/buckit`
- Tags: `:{release-tag}` and `:latest` (stable only, not for RCs)

### Job 3: Publish (depends on Build + Docker)

- Downloads all artifacts from the Build job
- Creates a GitHub Release via `softprops/action-gh-release`
- Attaches all binaries, packages, checksums, and signatures
- Marks as prerelease if tag contains `.rc`

## Trigger

```sh
# Stable release
git tag RELEASE.2026-05-08T23-00-00Z
git push origin RELEASE.2026-05-08T23-00-00Z

# Release candidate
git tag RELEASE.2026-05-08T23-00-00Z.rc1
git push origin RELEASE.2026-05-08T23-00-00Z.rc1
```

Release candidates:
- Skip the `:latest` Docker tag
- GitHub Release is marked as prerelease

## Secrets Required

| Secret | Purpose |
|--------|---------|
| `MINISIGN_PRIVATE_KEY` | Base64-encoded minisign private key |
| `MINISIGN_PASSWORD` | Passphrase for the private key |
| `DOCKERHUB_USERNAME` | Docker Hub username |
| `DOCKERHUB_TOKEN` | Docker Hub access token |
| `GITHUB_TOKEN` | Built-in — used for ghcr.io push and release creation |

## Setup Steps

1. Generate a minisign key pair:
   ```sh
   minisign -G -p buckit.pub -s buckit.key
   ```

2. Add repository secrets:
   - `MINISIGN_PRIVATE_KEY` = `base64 < buckit.key`
   - `MINISIGN_PASSWORD` = passphrase chosen during key generation
   - `DOCKERHUB_USERNAME` = Docker Hub username
   - `DOCKERHUB_TOKEN` = Docker Hub access token (create at https://hub.docker.com/settings/security)

3. Update `cmd/update.go` line 560 with the new public key (from `buckit.pub`)

4. Update Dockerfiles that reference the old MinIO public key (`RWTx5Zr1...`)

5. Push a tag to trigger the first release

## Tools

| Tool | Version | Purpose |
|------|---------|---------|
| Go | 1.25.x | Compilation |
| pkger | v2.3.11 | Generates .deb/.rpm/.apk from binary |
| minisign | v0.2.1 | Binary signing |
| docker buildx | latest | Multi-arch Docker image builds |

## Download URLs

After release, artifacts are available at:

- **Binaries**: `https://github.com/buckit-io/buckit/releases/download/{tag}/buckit-amd64.{tag}`
- **Packages**: `https://github.com/buckit-io/buckit/releases/download/{tag}/buckit_{version}_arm64.deb`
- **Docker**: `docker pull ghcr.io/buckit-io/buckit:{tag}` or `docker pull buckitio/buckit:{tag}`

## Permissions

```yaml
permissions:
  contents: write    # Create GitHub Releases
  packages: write   # Push to ghcr.io
```
