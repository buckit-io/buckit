# Self-Update Migration Plan

This document describes what needs to change so that `mc admin update` works
against a Buckit server downloading Buckit-hosted binaries. It is scoped to
the self-update flow only; general release mechanics are covered in
[`release-process.md`](./release-process.md) and
[`release-process-plan.md`](./release-process-plan.md).

## TL;DR

**There is no protocol-level blocker.** `mc` and `madmin-go` work unchanged
against Buckit. Everything required is either:

- a small code change in `cmd/update.go` / `cmd/build-constants.go`, or
- a deployment change (hosting the release files at a Buckit-controlled URL
  with the expected filenames).

If you keep the MinIO admin API path (`/minio/admin/v3/*`) and keep
minisign signatures, existing `mc` binaries continue to work against Buckit
servers with no user-visible changes beyond the URL they pass on the CLI.

## How the flow works end to end

1. User runs `mc admin update <alias> [<updateURL>]`.
2. `mc` sends `POST /minio/admin/v3/update?type=2&updateURL=<URL>` to one
   Buckit server (the coordinator). The request is signed with the alias's
   admin credentials.
3. The coordinator fetches `<URL>` (the `.sha256sum` file), parses
   `<sha> <releaseInfo>`, and checks that the release timestamp is newer than
   the running version.
4. The coordinator downloads the binary **once** from the directory derived
   from `<URL>` and holds it in memory, zstd-compressed.
5. The coordinator fans out to every peer via the internal
   `/peer/verifybinary` REST endpoint, pushing the compressed bytes and the
   original update URL.
6. **Each peer independently** re-fetches `<dir>/<releaseInfo>.minisig` from
   the update URL, verifies the sha256 and the signature against its
   configured minisign pubkey, and writes the new binary to a temp path.
7. Once every peer reports success, the coordinator calls
   `/peer/commitbinary` on each peer, which swaps the binary in place. The
   coordinator commits itself last and signals a service restart.

Implications:

- **Every cluster node** must reach `dl.buckit.io` (at least for `.minisig`).
- **Every cluster node** must agree on the minisign pubkey (compiled-in
  default or `MINIO_UPDATE_MINISIGN_PUBKEY` env).
- Bandwidth on the release host scales with number of clusters, not number of
  nodes in each cluster.

## Client side (`mc` + `madmin-go`) — no changes needed

Verified against current `master` of both repos:

| Concern | Result |
|---|---|
| Admin API path | `libraryAdminURLPrefix = "/minio/admin"`; Buckit still serves this |
| Protocol | Plain `POST` with query params; no MinIO-specific handshake |
| User-Agent | `MinIO (OS; ARCH) madmin-go/x.y.z`; server never inspects it |
| Version gating | None; madmin sends the request regardless of server version |
| URL argument | Optional 2nd positional arg to `mc admin update`; passed verbatim to the server |

Cosmetic only (users see but nothing breaks):

- CLI help: "update all MinIO servers".
- Confirmation prompt: "You are about to upgrade *MinIO Server*, please
  confirm [y/N]:".

If a rebrand of `mc` is desired it belongs in a separate fork; it is not
required for the self-update flow to work.

## Server side (this repo) — required changes

### 1. Accept `buckit.` release-info prefix

`cmd/update.go` has two hardcoded checks that reject anything not prefixed
with `minio.`:

```go
// parseReleaseData (~L342)
if nfields[0] != "minio" {
    err = fmt.Errorf("Unknown release `%s`", releaseInfo)
    ...
}

// releaseInfoToReleaseTime (~L370)
if nfields[0] != "minio" {
    err = fmt.Errorf("Unknown release `%s`", releaseInfo)
    ...
}
```

Both are called during the update flow — the first on the coordinator, the
second on every peer (via `peer-rest-server.go:VerifyBinaryHandler`).
Fixing only one leaves peer verification broken.

**Proposed fix:** accept both `buckit.` and `minio.` so older hosted
artifacts keep working, then drop `minio.` in a later release once hosting is
fully migrated.

```go
if nfields[0] != "buckit" && nfields[0] != "minio" {
    err = fmt.Errorf("Unknown release `%s`", releaseInfo)
    ...
}
```

### 2. Rotate the default minisign pubkey

`cmd/update.go` L558 hardcodes MinIO's pubkey:

```go
const (
    defaultMinisignPubkey = "RWTx5Zr1tiHQLwG9keckT0c45M3AGeHD6IvimQHpyRywVWGbP1aVSGav"
)
```

Replace with the Buckit pubkey generated during release-process setup
(`minisign -G -p buckit.pub -s buckit.key`).

### 3. Point the default release URL at GitHub Pages

`cmd/build-constants.go`:

```go
MinioReleaseBaseURL = "https://dl.min.io/server/minio/release/"
```

Change to:

```go
MinioReleaseBaseURL = "https://buckit-io.github.io/buckit/server/buckit/release/"
```

This is the URL used when `mc admin update play/` is run with no explicit
URL. The sha256sum file lives here; the binary is then fetched from GitHub
Releases (see change #3b below).

### 3b. Derive binary URL from GitHub Releases (not sibling directory)

Currently `cmd/update.go` assumes the binary is in the same directory as
the sha256sum file:

```go
u.Path = path.Dir(u.Path) + SlashSeparator + releaseInfo
```

When the default URL points at GitHub Pages, the binary is not there (Pages
has a 100 MB file limit). Add logic so that when using the default URL, the
binary download URL is constructed as:

```
https://github.com/buckit-io/buckit/releases/download/<tag>/buckit-<arch>.<tag>
```

When the user passes an explicit URL, preserve the existing sibling-directory
behavior for backward compatibility with private mirrors.

### 4. Fix the Docker image (handled by Phase 1 Dockerfile rewrite)

The root `Dockerfile` currently inherits `FROM minio/minio:latest`, which
sets `MINIO_UPDATE_MINISIGN_PUBKEY` to MinIO's key. Phase 1 task #5
replaces the base image with a minimal one, eliminating this problem
entirely.

### 5. Optional: add a Buckit-branded env override

`envMinisignPubKey = "MINIO_UPDATE_MINISIGN_PUBKEY"` still works as an
override, but operators looking for Buckit-specific knobs expect a
`BUCKIT_*` name. Consider accepting both:

```go
minisignPubkey := env.Get("BUCKIT_UPDATE_MINISIGN_PUBKEY",
    env.Get("MINIO_UPDATE_MINISIGN_PUBKEY", defaultMinisignPubkey))
```

Non-blocking.

## Hosting: GitHub Pages + GitHub Releases (zero cost)

Instead of running a dedicated download mirror (`dl.buckit.io` on S3/CDN),
we use two existing GitHub services with no rate limits on downloads:

- **GitHub Pages** (this repo's `gh-pages` branch) hosts the small
  `buckit.sha256sum` pointer file at a fixed URL per architecture.
- **GitHub Releases** hosts the actual binary and `.minisig` as release
  assets (no file-size limit on release assets; Pages has a 100 MB cap).

### URL layout

The default update URL (compiled into `MinioReleaseBaseURL`) points at
GitHub Pages:

```
https://buckit-io.github.io/buckit/server/buckit/release/linux-amd64/buckit.sha256sum
https://buckit-io.github.io/buckit/server/buckit/release/linux-arm64/buckit.sha256sum
```

Each `buckit.sha256sum` contains one line:

```
<sha256hex> buckit.RELEASE.2026-05-09T12-00-00Z
```

The server then derives the binary and signature URLs from GitHub Releases:

```
https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-09T12-00-00Z/buckit-amd64.RELEASE.2026-05-09T12-00-00Z
https://github.com/buckit-io/buckit/releases/download/RELEASE.2026-05-09T12-00-00Z/buckit-amd64.RELEASE.2026-05-09T12-00-00Z.minisig
```

This requires a small change in `cmd/update.go`: after parsing the
sha256sum content, construct the binary URL using the GitHub Releases
download pattern rather than assuming the binary is a sibling file in the
same directory as the sha256sum.

### Why this works

- **No rate limits.** GitHub Pages is a static CDN (Fastly). GitHub Releases
  asset downloads go through `objects.githubusercontent.com` — also no
  documented rate cap.
- **No hosting cost.** Both services are free for public repos.
- **No infra to manage.** No S3 buckets, CloudFront distributions, TLS
  certs, or DNS beyond what GitHub provides.
- **Explicit URL override still works.** If a user passes a URL to
  `mc admin update`, the existing sha256sum-file parsing logic is used
  as-is (flat-directory assumption preserved for private mirrors).

### Requirements

- HTTPS with valid cert: GitHub provides this automatically.
- `buckit.sha256sum` must reference the exact filename published as a
  GitHub Release asset.
- The release workflow must update the `gh-pages` branch sha256sum file on
  each stable release (not on RCs).

## Release workflow changes

Extend `.github/workflows/release.yml` to update the `gh-pages` branch
sha256sum pointer after each stable release. Example step:

```yaml
- name: Update gh-pages sha256sum pointer
  if: "!contains(github.ref_name, '.rc')"
  env:
    GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
  run: |
    for arch in amd64 arm64; do
      tag=${{ github.ref_name }}
      release_info="buckit.${tag}"
      asset="buckit-${arch}.${tag}"
      sha=$(sha256sum ${asset} | awk '{print $1}')
      mkdir -p pages/server/buckit/release/linux-${arch}
      echo "${sha} ${release_info}" > pages/server/buckit/release/linux-${arch}/buckit.sha256sum
    done

    cd pages
    git init
    git checkout -b gh-pages
    git add .
    git commit -m "Update sha256sum pointers for ${{ github.ref_name }}"
    git push --force https://x-access-token:${GITHUB_TOKEN}@github.com/buckit-io/buckit.git gh-pages
```

No additional secrets required — `GITHUB_TOKEN` already has `contents: write`
permission from the workflow's `permissions` block.

## Rollout order

1. **Code change** (this repo): accept `buckit.` prefix, rotate pubkey,
   repoint default URL, fix Dockerfile env. Ship as a Buckit release.
2. **Publish** that release to GitHub Releases only. Cut it as an RC first.
3. **Stand up `dl.buckit.io`** and backfill that same release to the new
   mirror.
4. **Update the release workflow** to publish to both GitHub Releases and
   `dl.buckit.io` going forward.
5. **Install the new build on a test cluster**, run
   `mc admin update <alias> https://dl.buckit.io/server/buckit/release/linux-<arch>/buckit.sha256sum`
   against it, and verify the rolling restart succeeds across all nodes.
6. **Cut the first stable release** using the full pipeline.

## Testing checklist

Before declaring self-update working:

- [ ] `minisign -V` manually verifies a downloaded binary against `buckit.pub`.
- [ ] `mc admin update <alias> <url>` with an older Buckit server upgrades
      to the newest release; `mc admin info` reports the new version.
- [ ] Same test against a 4-node distributed Buckit cluster: every node
      restarts on the new binary.
- [ ] `mc admin update --dry-run` reports the intended target without
      restarting.
- [ ] `mc admin update <alias>` (no URL) uses the compiled-in default URL
      successfully.
- [ ] An RC release uploaded to the `prerelease/` prefix is NOT picked up
      by `mc admin update <alias>` with no URL.
- [ ] Running `mc admin update` against a cluster that is already on the
      latest version returns "server is running the latest version" cleanly.
- [ ] Tampering with the hosted binary (flipping one byte) causes
      verification to fail on every peer and no commit happens.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Minisign private key leak | Offline backup in a password manager / secrets store; CI uses short-lived GitHub Actions secrets; key rotation requires a coordinated release |
| `dl.buckit.io` outage blocks updates | CDN with multi-region origin; self-update is non-critical and users can always re-run later |
| Pubkey compiled into old servers can't verify new binaries after a rotation | Plan key rotations well in advance; publish both old and new pubkeys temporarily; or cut a release that accepts both before rotating |
| Mixed-version cluster during rolling update | Existing cluster behavior — admin API already handles this |
| Network partition mid-update leaves some peers on old binary | `mc admin update` returns an error listing which peers failed; re-run safely re-attempts |

## Out of scope for this migration

- Rebranding `mc` CLI strings (separate fork).
- Replacing minisign with cosign / sigstore (tracked in
  `release-process-plan.md` as Phase 4).
- Removing the MinIO-branded env var names in favor of `BUCKIT_*` (cleanup
  pass, orthogonal).
- Any change to the admin API path `/minio/admin/v3/*` — breaking it would
  break `mc` compatibility.
