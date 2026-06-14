# FastOpen Docker Rig

This directory wraps the existing Docker cluster harness in
`testing/cluster/` for FastOpen A/B testing on a more realistic topology:

- `4` nodes
- `4` drives per node
- single distributed pool across all `16` drives
- node1 S3 API exposed on `127.0.0.1:9000`

The wrapper recreates the full cluster for each arm:

- `start-off` creates the cluster with `BUCKIT_FAST_GET=0`
- `start-on` creates the cluster with `BUCKIT_FAST_GET=1`

That matches the recent methodology where each measured arm is seeded with a
fresh object prefix before the GET pass.

## Commands

```sh
testing/fastopen-docker/rig.sh start-off
testing/fastopen-docker/rig.sh start-on
testing/fastopen-docker/rig.sh status
testing/fastopen-docker/rig.sh destroy
```

The public endpoint used by the existing Python runner is:

```sh
http://127.0.0.1:9000
```

## Example Flow

Create the OFF arm:

```sh
testing/fastopen-docker/rig.sh start-off
```

Seed and run against node1:

```sh
python3 testing/fastopen-local/fastopen_local.py seed \
  --host 127.0.0.1:9000 \
  --object-size 640KiB \
  --object-count 200 \
  --key-prefix obj-docker-off-640k- \
  --keys-output /private/tmp/fastopen-docker-off-640k.keys \
  --overwrite

python3 testing/fastopen-local/fastopen_local.py run \
  --host 127.0.0.1:9000 \
  --keys-file /private/tmp/fastopen-docker-off-640k.keys \
  --path-arm OFF \
  --object-size-label 640KiB \
  --cache-profile 1x-all \
  --ordering key-order \
  --concurrency 1 \
  --output-csv /private/tmp/fastopen-docker-off-640k.csv \
  --summary-json /private/tmp/fastopen-docker-off-640k.json
```

Switch to ON:

```sh
testing/fastopen-docker/rig.sh start-on
```

Then seed a fresh ON prefix and run the same GET plan.

## Optional HDD Delay

You can request per-drive device-mapper latency inside each Buckit node by
setting `HDD_DELAY_MS` before starting the rig:

```sh
HDD_DELAY_MS=8 testing/fastopen-docker/rig.sh start-off
```

This relies on the Linux kernel exposing the `dm-delay` target inside the
Docker environment.

Important limitation:

- On this macOS + Docker Desktop setup, the Docker VM kernel does not expose
  `dm-delay`, so `HDD_DELAY_MS` will fail fast during container startup.
- The feature is intended for a native Linux host or Linux VM where
  `dmsetup targets` includes `delay`.

## Optional Network Delay

You can add node-only egress network latency on `eth0` with `tc netem`:

```sh
NETEM_DELAY=25ms testing/fastopen-docker/rig.sh start-off
NETEM_DELAY=12us NETEM_JITTER=8us testing/fastopen-docker/rig.sh start-off
```

This delays traffic leaving each Buckit node container. In this rig that is
useful for simulating:

- Buckit node-to-node traffic
- Buckit response egress back to the benchmark client

This is not a symmetric full RTT emulator for host-to-container traffic; it is
node-only egress shaping.
