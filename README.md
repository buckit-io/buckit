# Buckit Quickstart Guide

[![license](https://img.shields.io/badge/license-AGPL%20V3-blue)](https://github.com/buckit-io/buckit/blob/master/LICENSE)

Buckit is a high-performance, S3-compatible object storage solution released under the GNU AGPL v3.0 license.
Designed for speed and scalability, it powers AI/ML, analytics, and data-intensive workloads with industry-leading performance.

- S3 API Compatible – Seamless integration with existing S3 tools
- Built for AI & Analytics – Optimized for large-scale data pipelines
- High Performance – Ideal for demanding storage workloads.

This README provides instructions for building Buckit from source and deploying onto baremetal hardware.

## Buckit is Open Source Software

We designed Buckit as Open Source software for the Open Source software community. We encourage the community to remix, redesign, and reshare Buckit under the terms of the AGPLv3 license.

All usage of Buckit in your application stack requires validation against AGPLv3 obligations, which include but are not limited to the release of modified code to the community from which you have benefited. Any commercial/proprietary usage of the AGPLv3 software, including repackaging or reselling services/features, is done at your own risk.

The AGPLv3 provides no obligation by any party to support, maintain, or warranty the original or any modified work.
All support is provided on a best-effort basis through Github.

## Source-Only Distribution

**Important:** The Buckit community edition is distributed as source code only.

### Installing Latest Buckit Community Edition

To use Buckit community edition, you have two options:

1. **Install from source** using `go install github.com/buckit-io/buckit@latest` (recommended)
2. **Build a Docker image** from the provided Dockerfile

See the sections below for detailed instructions on each method.

### Legacy Binary Releases

Historical pre-compiled binary releases remain available for reference but are no longer maintained:

- GitHub Releases: https://github.com/buckit-io/buckit/releases

**These legacy binaries will not receive updates.** We strongly recommend using source builds for access to the latest features, bug fixes, and security updates.

## Install from Source

Use the following commands to compile and run a standalone Buckit server from source.
If you do not have a working Golang environment, please follow [How to install Golang](https://golang.org/doc/install). Minimum version required is [go1.25](https://golang.org/dl/#stable)

```sh
go install github.com/buckit-io/buckit@latest
```

You can alternatively run `go build` and use the `GOOS` and `GOARCH` environment variables to control the OS and architecture target.
For example:

```
env GOOS=linux GOARCH=arm64 go build -tags kqueue
```

Start Buckit by running `buckit server PATH` where `PATH` is any empty folder on your local filesystem.

The Buckit deployment starts using default root credentials `buckitadmin:buckitadmin`.
You can test the deployment using the Buckit Console, an embedded web-based object browser built into Buckit Server.
Point a web browser running on the host machine to <http://127.0.0.1:9000> and log in with the root credentials.
You can use the Browser to create buckets, upload objects, and browse the contents of the Buckit server.

You can also connect using any S3-compatible tool, such as the Buckit Client `mc` commandline tool:

```sh
mc alias set local http://localhost:9000 buckitadmin buckitadmin
mc admin info local
```

> [!NOTE]
> Production environments using compiled-from-source Buckit binaries do so at their own risk.
> The AGPLv3 license provides no warranties nor liabilities for any such usage.

## Build Docker Image

You can use the `docker build .` command to build a Docker image on your local host machine.
You must first [build Buckit](#install-from-source) and ensure the `buckit` binary exists in the project root.

The following command builds the Docker image using the default `Dockerfile` in the root project directory with the repository and image tag `buckit:latest`

```sh
docker build -t buckit:latest .
```

Use `docker image ls` to confirm the image exists in your local repository.
You can run the server using standard Docker invocation:

```sh
docker run -p 9000:9000 -p 9001:9001 buckit:latest server /data --console-address :9001
```

Complete documentation for building Docker containers, managing custom images, or loading images into orchestration platforms is out of scope for this documentation.
You can modify the `Dockerfile` and `dockerscripts/docker-entrypoint.sh` as-needed to reflect your specific image requirements.

## Install using Helm Charts

There are two paths for installing Buckit onto Kubernetes infrastructure:

- Use the [Buckit Operator](https://github.com/buckit-io/operator)
- Use the community-maintained [Helm charts](https://github.com/buckit-io/buckit/tree/master/helm/minio)

## Test Buckit Connectivity

### Test using Buckit Console

Buckit Server comes with an embedded web based object browser.
Point your web browser to <http://127.0.0.1:9000> to ensure your server has started successfully.

> [!NOTE]
> Buckit runs console on random port by default, if you wish to choose a specific port use `--console-address` to pick a specific interface and port.

### Test using Buckit Client `mc`

`mc` provides a modern alternative to UNIX commands like ls, cat, cp, mirror, diff etc. It supports filesystems and Amazon S3 compatible cloud storage services.

The following commands set a local alias, validate the server information, create a bucket, copy data to that bucket, and list the contents of the bucket.

```sh
mc alias set local http://localhost:9000 buckitadmin buckitadmin
mc admin info
mc mb data
mc cp ~/Downloads/mydata data/
mc ls data/
```

## Contribute to Buckit Project

Please follow Buckit [Contributor's Guide](https://github.com/buckit-io/buckit/blob/master/CONTRIBUTING.md) for guidance on making new contributions to the repository.

## License

- Buckit source is licensed under the [GNU AGPLv3](https://github.com/buckit-io/buckit/blob/master/LICENSE).
- Buckit [documentation](https://buckit-io.github.io/docs) is licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
- [License Compliance](https://github.com/buckit-io/buckit/blob/master/COMPLIANCE.md)
