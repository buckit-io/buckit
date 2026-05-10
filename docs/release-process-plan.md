# Buckit Release Process — Implementation Plan

This plan describes how to bring the release process defined in
[`release-process.md`](./release-process.md) into working state. It is a
companion to that design doc: the design specifies *what* the system should do;
this plan specifies *what needs to change* in the current repo, in what order,
and how to validate each step.

## Gap analysis (design vs. current state)

Much of the design is already scaffolded in `.github/workflows/release.yml`.
Below is what is done, what is missing, and what is inconsistent with the
design.

| Area | Design | Current | Status |
|---|---|---|---|
| Workflow file exists | `release.yml` with 3 jobs | Present | ✅ |
| Tag trigger `RELEASE.*` | yes | yes | ✅ |
| Build matrix amd64/arm64 | yes | yes | ✅ |
| Minisign sign (v0.2.1) | yes | yes | ✅ |
| pkger (v2.3.11) `.deb`/`.rpm`/`.apk` | yes | yes | ✅ |
| Docker multi-arch from root `Dockerfile` | yes | yes | ✅ |
| Push to `ghcr.io/buckit-io/buckit` | yes | yes | ✅ |
| Push to `docker.io/buckitio/buckit` | yes | **missing** (no Docker Hub login, no Docker Hub tags) | ❌ |
| `:latest` only for stable (not `.rc`) | yes | yes | ✅ |
| GitHub Release w/ prerelease on `.rc` | yes | yes | ✅ |
| `cmd/update.go` uses Buckit minisign pubkey | yes | still old MinIO key `RWTx5Zr1...` | ❌ |
| Legacy Dockerfiles updated/retired | yes | `Dockerfile.release`, `Dockerfile.release.old_cpu`, `Dockerfile.hotfix` still reference MinIO key + `dl.min.io` | ❌ |
| Required secrets configured in repo | `MINISIGN_*`, `DOCKERHUB_*` | not yet set | ❌ (out-of-band; requires repo owner) |

Unrelated but risky findings flagged for confirmation before the first release:

- Root `Dockerfile` uses `FROM minio/minio:latest` — a "buckit" release image
  inheriting from MinIO's upstream runtime. The design does not mention
  rebasing, but this is worth an explicit decision before tagging `:latest`.
- `dockerscripts/docker-entrypoint.sh` still uses `MINIO_USERNAME` /
  `MINIO_GROUPNAME` env vars. This is orthogonal to release mechanics and can
  be left unless a rebrand pass is scheduled.

## Plan (ordered)

### Phase 1 — Code and workflow changes (reversible, in-repo)

1. **Update `.github/workflows/release.yml` — Docker job**
   - Add a `docker/login-action@v3` step for `docker.io` using
     `${{ secrets.DOCKERHUB_USERNAME }}` / `${{ secrets.DOCKERHUB_TOKEN }}`.
   - Extend the "Determine tags" step to also emit:
     - `docker.io/buckitio/buckit:${{ github.ref_name }}`
     - `docker.io/buckitio/buckit:latest` (only when tag does not contain `.rc`)
   - The `docker/build-push-action@v5` already consumes the combined tag list —
     no structural change needed.

2. **Rotate minisign public key in code (`cmd/update.go`)**
   - Replace the `defaultMinisignPubkey` constant (~line 560) with the new
     Buckit public key produced in Phase 2, step 1.
   - Keep the `envMinisignPubKey` override path intact.
   - Update any test fixture in `cmd/update_test.go` that hardcodes the key.

3. **Update or retire legacy Dockerfiles**
   - `Dockerfile.release`, `Dockerfile.release.old_cpu`, `Dockerfile.hotfix` all:
     - download binaries from `https://dl.min.io/...` (MinIO infrastructure —
       will not serve Buckit),
     - verify against the old MinIO minisign key,
     - embed `MINIO_UPDATE_MINISIGN_PUBKEY=RWTx5Zr1...` in the image env.
   - Options (pick one):
     - **(A) Delete them.** The automated release uses the root `Dockerfile`
       only; the hotfix flow (`make hotfix`) pushes to `dl.buckit.io` which
       does not exist yet anyway. Simpler, avoids a broken build matrix.
     - **(B) Repoint them** at a future Buckit release URL and pubkey once
       download infrastructure exists. More code; no value until an equivalent
       `dl.buckit.io` mirror is provisioned.
   - Recommendation: **A** for now; revisit when/if a public binary mirror goes
     up.

4. **Sanity-check root `Dockerfile`**
   - Decide whether `FROM minio/minio:latest` is acceptable for a v1 release or
     should be replaced with a minimal base (e.g., `ubi9/ubi-micro` or
     `alpine`) that just ships the `buckit` binary. This is the image the
     release will publicly tag as `:latest`.
   - If replacing, also bring over the `CREDITS` / `LICENSE` /
     `docker-entrypoint.sh` copies that `Dockerfile.release` performed.

5. **Document the release procedure**
   - Add a short `RELEASE.md` (or section in `README.md`) covering the tag
     format (`RELEASE.YYYY-MM-DDTHH-MM-SSZ[.rcN]`) and the single command
     developers run to cut a release.
   - Cross-link from `docs/release-process.md`.

### Phase 2 — Owner setup tasks (out-of-band, one-time)

These need access to the GitHub org, Docker Hub, and a local machine with
`minisign` installed.

1. **Generate the Buckit minisign keypair**
   ```sh
   minisign -G -p buckit.pub -s buckit.key
   ```
   Store `buckit.key` and its passphrase in a password manager. Publish
   `buckit.pub` in the repo (e.g., `SECURITY.md` or a `buckit.pub` file at the
   root) so users can verify releases.

2. **Configure repository secrets** (Settings → Secrets and variables → Actions)
   - `MINISIGN_PRIVATE_KEY` = `base64 -w0 < buckit.key`
   - `MINISIGN_PASSWORD` = passphrase from step 1
   - `DOCKERHUB_USERNAME` = Docker Hub user that owns `buckitio/buckit`
   - `DOCKERHUB_TOKEN` = Docker Hub access token scoped to `Read, Write, Delete`
     on that repo
   - `GITHUB_TOKEN` is built-in — nothing to add.

3. **Pre-create Docker Hub repo** `docker.io/buckitio/buckit` (public) if it
   does not already exist.

4. **Confirm ghcr.io package visibility** — after the first push, make the
   ghcr package public in GitHub Package settings.

### Phase 3 — Dry run and first release

1. **Local verification of build tooling** (no secrets needed):
   ```sh
   BUCKIT_RELEASE=RELEASE go run buildscripts/gen-ldflags.go \
     RELEASE.2026-05-08T23-00-00Z

   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags kqueue -trimpath \
     --ldflags "$(BUCKIT_RELEASE=RELEASE go run buildscripts/gen-ldflags.go \
                  RELEASE.2026-05-08T23-00-00Z)" \
     -o /tmp/buckit-test

   /tmp/buckit-test --version
   ```
   Confirms the tag format parses and ldflags wire through.

2. **Cut a release candidate first** to exercise the prerelease path without
   claiming `:latest`:
   ```sh
   git tag RELEASE.$(date -u +%Y-%m-%dT%H-%M-%SZ).rc1
   git push origin RELEASE.*.rc1
   ```
   Validate:
   - All three jobs succeed.
   - GitHub Release exists and is flagged `prerelease`.
   - `.deb`, `.rpm`, `.apk`, `.sha256sum`, `.minisig`, and raw binaries are all
     attached for amd64 and arm64.
   - `ghcr.io/buckit-io/buckit:RELEASE.*.rc1` exists for both platforms:
     `docker manifest inspect ...`.
   - `docker.io/buckitio/buckit:RELEASE.*.rc1` exists.
   - Neither registry has `:latest` updated.
   - Signature verifies locally:
     ```sh
     minisign -Vm <binary> -x <binary>.minisig -P <buckit.pub contents>
     ```

3. **Cut the first stable release** once the RC passes all checks.

### Phase 4 — Follow-ups (optional, post-MVP)

- Add an SBOM step (`anchore/sbom-action`) and attach it to the GitHub Release.
  Commonly requested for AGPL-licensed infrastructure software.
- Add `cosign` image signing (independent of minisign binary signing).
- Publish Helm chart index updates from the same workflow (there is already a
  `helm-reindex.sh` at the repo root).
- Rename `MINIO_*` env variables in `dockerscripts/docker-entrypoint.sh` to
  `BUCKIT_*` (separate rebrand task, not release-process).

## Risk & rollback

- All Phase 1 changes are reversible via `git revert`.
- A failed first release only creates an orphaned GitHub Release + container
  tags; delete the release and the immutable image tags without affecting
  anything else.
- The **only irreversible** action is publishing the minisign public key. Once
  users pin to it, rotating requires a coordinated key-rotation plan. Keep the
  private key offline after provisioning.

## Deliverables summary

| File | Change |
|---|---|
| `.github/workflows/release.yml` | Add Docker Hub login + tags |
| `cmd/update.go` | Replace `defaultMinisignPubkey` |
| `cmd/update_test.go` | Update pubkey fixture if present |
| `Dockerfile.release`, `Dockerfile.release.old_cpu`, `Dockerfile.hotfix` | Delete (recommended) or repoint |
| `Dockerfile` (root) | Confirm base image is acceptable; optionally rebase |
| `buckit.pub` (new) | Publish Buckit minisign public key |
| `README.md` / new `RELEASE.md` | Document tag format + workflow |
