> [!NOTE]
> Buckit is an independent project derived from the discontinued open source
> [MinIO](https://github.com/minio/minio) project. Buckit is not affiliated
> with or endorsed by MinIO, Inc.

# Buckit Object Storage

<p align="center">
  <a href="https://github.com/buckit-io/buckit/actions/workflows/go-cross.yml">
    <img src="https://github.com/buckit-io/buckit/actions/workflows/go-cross.yml/badge.svg" alt="Build status">
  </a>
  <a href="https://github.com/buckit-io/buckit/actions/workflows/vulncheck.yml">
    <img src="https://github.com/buckit-io/buckit/actions/workflows/vulncheck.yml/badge.svg" alt="Vulnerability check">
  </a>
  <a href="https://buckit.sh">
    <img src="https://img.shields.io/badge/Website-buckit.sh-111827?logo=googlechrome&logoColor=white" alt="Buckit website">
  </a>
  <a href="https://discord.gg/v5utBPpsGu">
    <img src="https://img.shields.io/badge/Discord-Join%20the%20community-5865F2?logo=discord&logoColor=white" alt="Join Buckit on Discord">
  </a>
</p>

Buckit is an open-source, S3-compatible object storage system you run on your
own commodity servers. It gives your applications an Amazon S3-style API while
keeping your data under your control, and it can scale from a small setup to
hundreds of petabytes. Teams use Buckit when they want S3-like storage without
sending all data to a public cloud or paying cloud storage bills at scale.

<p align="center">
  <img src=".github/web-console.gif" alt="Buckit web console">
</p>

## Learn More

<a href="https://buckit.sh/#showcase-video" target="_blank" rel="noopener noreferrer">Watch demo videos</a><br>
<a href="https://buckit.sh/#getting-started" target="_blank" rel="noopener noreferrer">Getting Started</a><br>
<a href="https://buckit.sh/#faq" target="_blank" rel="noopener noreferrer">FAQ</a><br>
<a href="https://buckit.sh/docs" target="_blank" rel="noopener noreferrer">Documentation</a><br>
<a href="https://buckit.sh/blog/why-i-forked-minio-to-keep-it-open-and-made-it-faster" target="_blank" rel="noopener noreferrer">Blog: Why I Forked MinIO to Keep It Open, and Made It Faster</a>

<br>

> [!TIP]
> **Already running MinIO?** Buckit is a drop-in replacement. Read
> [MinIO to Buckit: Swap One File, Nothing Else](https://buckit.sh/blog/minio-to-buckit-swap-one-file).

## What Buckit Provides

- **S3-compatible object storage** — works with existing S3 SDKs and tools.
- **Standalone mode** — single-node for small setups or homelabs.
- **Distributed mode** — multi-node clusters supporting hundreds of petabytes.
- **Replication** — keep remote sites in sync for high availability and
  disaster recovery.
- **Single binary** — one executable per node, with no external dependencies.
- **Browser console** — self-service bucket and object management for end
  users.
- **Operations toolkit** — CLI for admin and object automation, web-based
  cluster and node management.

## Quickstart

From nothing to a file in object storage. No toolchain and no container
runtime required.

**1. Download the server.** The script verifies the published checksum and
leaves a `buckit` executable in the current directory:

```sh
curl -fsSL https://buckit-io.github.io/buckit/install-linux-binary.sh | sh
```

On macOS use `install-mac.sh`, on Windows `install-windows.ps1`. See
[Install Buckit](#install-buckit) for packages and other options, or
[Run with Docker](#run-with-docker) for a container.

**2. Start the server.**

```sh
./buckit server /tmp/buckit-data --console-address :9001
```

**3. Open the console** at <http://127.0.0.1:9001> and sign in with
`buckitadmin` / `buckitadmin`. You can create buckets and upload objects from
the browser. The S3 API itself listens on <http://127.0.0.1:9000>.

**4. Or use the command line.** `bm` is the Buckit client. It installs to
`~/.local/bin`, so add that to your `PATH`:

```sh
curl -fsSL https://buckit-io.github.io/bm/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

Point it at the server, create a bucket, and upload a file:

```sh
bm alias set local http://localhost:9000 buckitadmin buckitadmin
bm mb local/mydata
bm cp ./hello.txt local/mydata/
bm ls local/mydata
```

On Windows PowerShell, install `bm` with
`irm https://buckit-io.github.io/bm/install.ps1 | iex`.

### A whole cluster is one command too

```sh
buckit server http://node{1...4}.example.com/data{1...4}
```

That single line describes the entire deployment: four servers, four drives
each, sixteen drives in all. Run it unchanged on every node. There is no
coordinator process to stand up, no config file to distribute, and no external
database to operate.

Set root credentials on every node before starting, and keep them identical
across the cluster:

```sh
export MINIO_ROOT_USER=myadmin
export MINIO_ROOT_PASSWORD=mysecretpassword
```

The variable names keep the `MINIO_` prefix so existing deployment tooling
works unchanged.

For production clusters, [`bm web`](#buckit-manager-web) handles host
discovery, drive selection, preflight checks, and service setup for you.

## Install Buckit

For detailed installation instructions, see the
[Deployment Guide](https://buckit.sh/docs/operations/deployments/baremetal-deploy-server).

### Linux Packages (recommended)

Linux release packages are available as `.deb`, `.rpm`, and `.apk` artifacts.
The helper script downloads the package for the current system, verifies its
SHA-256 checksum, and prints the package-manager command to run:

```sh
curl -fsSL https://buckit-io.github.io/buckit/install-linux.sh | sh
```

### Linux Standalone Binary

To run the server without a package or a systemd service — for example when
taking over an existing deployment that is started by hand — download the
binary on its own. The helper script verifies its SHA-256 checksum and leaves
an executable `buckit` in the current directory:

```sh
curl -fsSL https://buckit-io.github.io/buckit/install-linux-binary.sh | sh
```

### Build From Source

Buckit requires Go 1.25 or newer. If Go is not installed, download and install
it from the official Go installation page: <https://go.dev/doc/install>.

```sh
git clone https://github.com/buckit-io/buckit.git
cd buckit
make build
```

The build writes `./buckit`.

You can also install directly with Go:

```sh
go install github.com/buckit-io/buckit@latest
```

When building manually, include the `kqueue` build tag:

```sh
go build -tags kqueue -trimpath --ldflags "$(go run buildscripts/gen-ldflags.go)" -o buckit
```

## Run with Docker

```sh
docker run -p 9000:9000 -p 9001:9001 \
  -v "$HOME/buckit-data:/data" \
  ghcr.io/buckit-io/buckit:latest server /data --console-address :9001
```

With explicit credentials:

```sh
docker run -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=myadmin \
  -e MINIO_ROOT_PASSWORD=mysecretpassword \
  -v "$HOME/buckit-data:/data" \
  ghcr.io/buckit-io/buckit:latest server /data --console-address :9001
```

The same image is published to `ghcr.io/buckit-io/buckit` and
`docker.io/buckitio/buckit`. For production, pin a release tag instead of
`latest`.

To build a custom image, use the release workflow or adapt the root
`Dockerfile` for your own artifact pipeline.

## Use Buckit With `bm`

`bm` is the Buckit Manager CLI for Buckit and S3-compatible object storage.
It provides object commands such as `ls`, `cp`, `cat`, `mirror`, and `rm`, plus
Buckit administration and cluster-management workflows.

See the [`bm` GitHub repository](https://github.com/buckit-io/bm).

Install `bm` on macOS or Linux:

```sh
curl -fsSL https://buckit-io.github.io/bm/install.sh | sh
bm --help
```

Install `bm` on Windows PowerShell:

```powershell
irm https://buckit-io.github.io/bm/install.ps1 | iex
bm --help
```

Add an alias for a local Buckit server:

```sh
bm alias set local http://localhost:9000 buckitadmin buckitadmin
bm admin info local
```

Create a bucket and copy data:

```sh
bm mb local/mydata
bm cp --recursive ./mydata/ local/mydata/
bm ls local/mydata
```

Read, mirror, and remove objects:

```sh
bm cat local/mydata/object.txt
bm mirror ./mydata local/mydata
bm rm local/mydata/object.txt
```

Use `--dry-run` before large or recursive remove operations:

```sh
bm rm --recursive --dry-run local/mydata
```

Common `bm` commands:

| Command | Purpose |
| --- | --- |
| `bm alias set` | Add or update a Buckit or S3-compatible endpoint alias. |
| `bm admin info` | Show deployment information and verify connectivity. |
| `bm ls` | List buckets, prefixes, objects, or local files. |
| `bm mb` | Create a bucket or local directory. |
| `bm cp` | Copy files or objects. |
| `bm cat` | Print file or object contents. |
| `bm mirror` | Synchronize local and object storage paths. |
| `bm rm` | Remove files or objects. |
| `bm version` | Manage bucket versioning. |
| `bm retention` | Manage object retention. |
| `bm legalhold` | Manage object legal holds. |
| `bm tag` | Manage object tags. |
| `bm ilm` | Manage lifecycle rules and tiers. |
| `bm replicate` | Manage bucket replication. |
| `bm update` | Update the `bm` binary. |

Full `bm` CLI documentation: <https://buckit.sh/docs/reference/bm-cli>

## Buckit Manager Web

`bm` also ships Buckit Manager Web, a local web UI for deploying and managing
Buckit clusters.

Start it with:

```sh
bm web
```

By default, Buckit Manager opens at `http://127.0.0.1:9443/`.

Use Buckit Manager Web to:

- Prepare a local single-node Buckit deployment on macOS or Windows.
- Deploy a managed Buckit cluster on one or more Linux servers over SSH.
- Import an existing Buckit or MinIO cluster.
- Monitor cluster health, nodes, pools, and drives.
- Run supported cluster and node operations.

Buckit Manager documentation:
<https://buckit.sh/docs/administration/buckit-manager>

## How Buckit Works Internally

- [Architecture](https://buckit.sh/docs/operations/concepts/architecture)
- [Erasure Coding](https://buckit.sh/docs/operations/concepts/erasure-coding)
- [Availability and Resiliency](https://buckit.sh/docs/operations/concepts/availability-and-resiliency)

## Development

Common repository commands:

```sh
make build
make install
make test
make lint
make verifiers
go generate ./...
make check-gen
```

Build and test commands must include the `kqueue` tag when run manually:

```sh
CGO_ENABLED=0 go test -v -tags kqueue,dev ./...
```

Generated files ending in `_gen.go` or `_string.go` must be regenerated and
committed when their source types change.

## License and Support

Buckit is licensed under the
[GNU AGPLv3](https://github.com/buckit-io/buckit/blob/master/LICENSE).

All usage must comply with AGPLv3 obligations. The license provides no warranty,
liability, or support obligation. Community support is available through GitHub
and the [Buckit Discord community](https://discord.gg/v5utBPpsGu).

See also:

- [Contributor Guide](https://github.com/buckit-io/buckit/blob/master/CONTRIBUTING.md)
- [License Compliance](https://github.com/buckit-io/buckit/blob/master/COMPLIANCE.md)
- [Buckit Manager CLI](https://github.com/buckit-io/bm)
- [Buckit Discord](https://discord.gg/v5utBPpsGu)
