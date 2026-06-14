# FastOpen Local Rig

This directory contains a local rig for the FastOpen Phase 1 performance plan in:

- `docs/fastopen-local-performance-test-plan.md`

It is intentionally separate from `testing/singletrip-hdd/`, which is the older
remote/HDD-oriented rig.

## Files

- `rig.sh`
  - builds `/private/tmp/buckit-fastopen`
  - creates `/private/tmp/buckit-fastopen-{1..16}`
  - starts or stops a local 16-path server with `BUCKIT_FAST_GET=0|1`
  - exposes metrics publicly by default via `MINIO_PROMETHEUS_AUTH_TYPE=public`
  - sets `MINIO_CI_CD=1` by default so local `/private/tmp` paths are accepted on macOS
  - forces `MINIO_STORAGE_CLASS_STANDARD=EC:4`
- `fastopen_local.py`
  - seeds deterministic object names such as `obj-000001`
  - runs controlled GET plans with exact cache/reuse profiles
  - records per-request CSV and summary JSON
  - snapshots FastOpen metrics before and after each run

## First-Pass Workflow

Build and reset the local rig:

```sh
testing/fastopen-local/rig.sh build
testing/fastopen-local/rig.sh wipe
testing/fastopen-local/rig.sh init-dirs
```

Start the `OFF` arm:

```sh
testing/fastopen-local/rig.sh start-off
```

Seed deterministic `640KiB` objects:

```sh
python3 testing/fastopen-local/fastopen_local.py seed \
  --host 127.0.0.1:9000 \
  --object-size 640KiB \
  --object-count 200 \
  --keys-output /private/tmp/fastopen-640k.keys
```

Run the first `1x all` pass at concurrency `1`:

```sh
python3 testing/fastopen-local/fastopen_local.py run \
  --host 127.0.0.1:9000 \
  --keys-file /private/tmp/fastopen-640k.keys \
  --path-arm OFF \
  --object-size-label 640KiB \
  --cache-profile 1x-all \
  --ordering key-order \
  --concurrency 1 \
  --output-csv /private/tmp/fastopen-off-640k-1x.csv \
  --summary-json /private/tmp/fastopen-off-640k-1x.json \
  --metrics-before /private/tmp/fastopen-off-640k-1x.metrics.before.json \
  --metrics-after /private/tmp/fastopen-off-640k-1x.metrics.after.json
```

Restart the same data set with the `ON` arm:

```sh
testing/fastopen-local/rig.sh start-on
```

Replay the same request plan:

```sh
python3 testing/fastopen-local/fastopen_local.py run \
  --host 127.0.0.1:9000 \
  --keys-file /private/tmp/fastopen-640k.keys \
  --path-arm ON \
  --object-size-label 640KiB \
  --cache-profile 1x-all \
  --ordering key-order \
  --concurrency 1 \
  --output-csv /private/tmp/fastopen-on-640k-1x.csv \
  --summary-json /private/tmp/fastopen-on-640k-1x.json \
  --metrics-before /private/tmp/fastopen-on-640k-1x.metrics.before.json \
  --metrics-after /private/tmp/fastopen-on-640k-1x.metrics.after.json
```

Do not wipe the directories between OFF and ON if you want a true A/B replay on
the same object set.

## Topology

The local rig is configured as:

- `1` pool
- `1` erasure set
- `16` local drives: `/private/tmp/buckit-fastopen-{1..16}`
- standard storage class parity forced to `EC:4`

That gives a `12 data + 4 parity` layout for STANDARD-class objects.

## Cache Profiles

Supported `--cache-profile` values:

- `1x-all`
- `2x-all`
- `10pct-hot`
- `20pct-hot`
- `50pct-hot`
- `hotset-5x`
- `hotset-10x`

Supported `--ordering` values:

- `key-order`
- `immediate-repeat`
- `pass-repeat`
- `shuffled`

For `hotset-*` profiles, the script uses the first `10%` of keys by default as
the hot set. Override with `--hot-fraction`.

## Output Shape

Per-request CSV columns:

- `run_id`
- `path_arm`
- `object_size`
- `cache_profile`
- `ordering`
- `concurrency`
- `request_index`
- `key`
- `access_number`
- `access_class`
- `ttfb_ms`
- `total_ms`
- `bytes`
- `status`
- `error`

Summary JSON includes:

- grouped stats for `all_requests`, `first_access`, and `repeated_access`
- `count`, `min`, `max`, `mean`, `p50`, `p90`, `p99` for both `ttfb_ms` and `total_ms`
- bytes read and error counts
- FastOpen metrics snapshots and deltas

## Notes

- The rig listens on `127.0.0.1:9000` by default, with the console on `127.0.0.1:9001`.
- The metrics endpoint defaults to `http://127.0.0.1:9000/minio/metrics/v3/api/requests`.
- The rig assumes local credentials `buckitadmin/buckitadmin`.
- The seeder and runner use direct SigV4 requests from Python stdlib; no `mc`,
  `aws`, or extra Python packages are required.
