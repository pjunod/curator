# Smart Media Discovery M0b: standalone helper packaging evidence

Status: **M0b packaging accepted by the originating reviewer**. This is
the packaging experiment authorized after M0a, not an enabled feature or a
release candidate. The owner chose a Curator-owned helper that does not require
Cinema. M1–M5 remain subject to the review gates in
[the plan](plan-smart-media-discovery.md).

The originating reviewer accepted the three local corrections in `030fee0`
and independently reran both the five-case vector comparison and the
maximum-escape transport request in one helper process. The reviewer's
maximum component difference was 0.00000016481557846 against the PyTorch
fixture; all vectors were finite and 384-dimensional. The separate Linux image
measurements below were accepted for M0b feasibility, with their workload and
base-image-cache limits retained.

## Reproduce

The isolated crate is `tools/curator-embed/`. Its Rust toolchain is 1.97.1,
the lockfile is committed, and it has no root Cargo workspace. Run:

```sh
make embed-check
make embed-build
CURATOR_EMBED_MODEL_DIR=/path/to/verified/model \
  cargo test --manifest-path tools/curator-embed/Cargo.toml \
  --locked -- --ignored --nocapture
make embed-image
DOCKER_HOST=ssh://pjunod@nuc3 make embed-image-smoke \
  MODEL_DIR=/absolute/path/on/docker-host/to/verified/model
```

The ignored tests check both Cinema tokenizer IDs and independent numeric
vectors. Their reference fixture is regenerated with Python 3.12 and the
versions in `tools/curator-embed/requirements-reference.txt`:

```sh
python tools/curator-embed/reference-vectors.py \
  /path/to/verified/model \
  tools/curator-embed/testdata/reference_vectors.json
```

The image command builds the proposed Linux x64 final image from
`deploy/Dockerfile.discovery-m0b`; it does not install or download a model.
The smoke command checks image user/platform, mounts the three verified model
files read-only at `/model`, runs with no network and a read-only root
filesystem, checks independent numeric vectors and the maximum escaped
request, then removes the container. `MODEL_DIR` is resolved on the Docker
daemon host.

The model is the pinned `sentence-transformers/all-MiniLM-L6-v2` revision
`1110a243fdf4706b3f48f1d95db1a4f5529b4d41`. The three expected SHA-256
digests, file sizes, and license are in
`tools/curator-embed/model-manifest.json`. The helper verifies all three before
loading. Model assets are temporary test inputs and are not committed or
baked into the image.

## Evidence recorded 2026-09-29

| Check | Result | Limit |
| --- | --- | --- |
| Locked local Rust build | `cargo test --locked` passed 4 protocol and verified-read tests; `cargo clippy --locked --all-targets -- -D warnings` and `cargo fmt --check` passed | macOS arm64, not final image |
| Release build | 1m 07s initial local build, 15s after protocol edit | macOS arm64 with local dependency cache; not Linux cold/warm Docker timing |
| Model manifest | Config 612 B, tokenizer 466,247 B, weights 90,868,376 B; all three SHA-256 values matched | Downloaded into local temporary storage |
| Tokenizer parity | 136/136 recorded Cinema corpus inputs matched exact token IDs | The three additional programmatic Cinema inputs were not copied into this fixture |
| Numeric vector parity | Five PyTorch 2.5.1 / Transformers 4.46.3 reference inputs matched: 16, 11, 11, 256, and 4 token cases; maximum per-component error 0.000000166 against a 0.00001 test tolerance | Final-image repeat is recorded below |
| Real local encoding | Eight texts returned eight finite 384-vectors; L2 norms 1.0–1.0000002; wrong model ID returned `bad_request`. A worst-case escaped 393,514-byte request with eight decoded 8 KiB NUL strings returned eight 384-vectors | Final-image smoke is recorded below |
| Local resources | Eight-text request, including process startup and model load: 0.307s; peak child RSS 187.6 MiB | One macOS arm64 observation, not a p95 |

The helper uses Candle 0.11, tokenizers 0.22 with onig, 256-token truncation,
unmasked token mean pooling, L2 normalization, and a two-thread Rayon pool,
matching Cinema's inspected `semantic.rs` encoder shape. It accepts at most
eight texts of 8 KiB each, bounds request lines at 394,240 bytes (worst-case
JSON escaping plus header allowance) and response lines
at 256 KiB, and keeps stdout for JSON protocol responses.
Model files are read once with an expected-size-plus-one cap, hashed, then
parsed from those verified bytes. `.dockerignore` excludes the local Rust
`target/` directory from both Docker stages.

## Linux x64 final-image measurements

The experimental Dockerfile has a Debian 12 Rust stage and a proposed
`gcr.io/distroless/cc-debian12` final stage with the existing static Go binary
and a dynamic Rust helper. After the owner explicitly authorized a temporary
nuc3 test, Docker transferred a 6.02 MB build context. The local 2.4 GB Rust
`target/` directory was excluded. No source checkout was created on nuc3.

| Check on nuc3, Linux x86_64 | Result | Interpretation |
| --- | --- | --- |
| No-cache Docker build | 99.09s; Rust release stage 1m 34s | Below the existing 20-minute image job; base images and registry downloads were already available, so this is not a pristine-host pull measurement |
| Cached Docker rebuild | 3.24s | Same source and image recipe |
| Loader and libraries | `/lib64/ld-linux-x86-64.so.2`; `libgcc_s.so.1`, `libm.so.6`, `libc.so.6` | Inspected with `ldd`/`readelf` in Debian 12 build stage; actual final-image execution proves the runtime closure for this binary |
| Final-image smoke | Image inspected as `linux/amd64`, user `1000:1000`; helper ran with no network, read-only root and model mount, 512 MiB memory limit; five reference vectors matched with max component error 0.000000195939496; escaped eight-text request returned eight 384-vectors | Real Candle inference in `gcr.io/distroless/cc-debian12`, not a builder-stage run |
| Helper-only container | 102.8 MiB sampled after requests | Current usage sample, not a peak |
| Monarr plus helper in one container | Helper `VmHWM` 190,216 KiB (185.8 MiB); cgroup `memory.peak` 194.2 MiB; cgroup current 113.1 MiB after requests | Both processes ran together as uid 1000 under a 512 MiB cgroup limit; not a search-workload p95 |

The Go container and all helper smoke containers were stopped. The temporary
model directory and `monarr:discovery-m0b` image tag were removed from nuc3;
a final check found no running `monarr-m0b` test container. Docker's normal
builder cache remains on the shared host and was not pruned.

M0b packaging evidence was accepted by the originating reviewer. M0c still has
to measure actual candidate quality and one-cold-search resource contention
before M1 can begin. The final feature has not been built or validated.
