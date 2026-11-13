# Dependency patches

Two upstream dependencies had no fixed release during the 2026-11-13 audit.
Curator carries narrow, reviewable replacements rather than suppressing their
advisories. These are local fixes, not claims that upstream has released patches.

## JavaScript merge tooling

`web/vendor/deepmerge` contains the published deepmerge 4.3.1 CommonJS
implementation and MIT license. `UPSTREAM.json` records the original file hash,
source archive and GHSA-gcqx-w48m-w2h4. The local package has its own honest name
and version. A parent-scoped Rollup override resolves its `deepmerge` import to
this package; it remains build tooling rather than a browser runtime dependency.

Three destination writes now use `Object.defineProperty` with enumerable,
writable and configurable data properties. A null-prototype merge target and a
target carrying its own `__proto__` key cannot invoke the inherited prototype
setter. Existing unsafe-key guards and merge options remain supported.

`npm test` runs Node regression cases before Vitest. Three poisoning cases fail
against the original implementation and pass with the patch. Other cases cover
nested merge, arrays, symbols, cloning, custom callbacks and special objects.
Use CI's Node 22/npm 10 to regenerate the lockfile, then run `npm ci`, `npm test`
and `npm run build` in `web`. Check `npm ls deepmerge` to verify Rollup resolves
the local compatibility package.

## Rust macro maintenance

`tools/curator-embed/vendor` contains ten checksum-verified published crates:
seven GEMM 0.19.0 crates, pulp 0.22.3, and tokenizers 0.22.2 and 0.23.2. Their
implementation files and licenses are unchanged. Each normalized Cargo manifest
changes only the `paste` dependency to package `pastey` 0.2.3, retaining the
`paste` import alias. Root Cargo patches select these local crates. The lockfile
contains `pastey` and no longer contains the unmaintained `paste` package
(RUSTSEC-2024-0436). This advisory describes maintenance risk rather than a
known runtime exploit.

`vendor/UPSTREAM.json` records each source archive and verified SHA-256. Both
Dockerfiles copy vendored dependencies before their locked builds. Run encoder
unit tests and the ignored tokenizer/vector parity tests with the pinned model
files, using `CURATOR_EMBED_MODEL_DIR=/path/to/verified/model cargo test --locked -- --ignored`
from `tools/curator-embed`.

## Maintenance and removal

When updating a vendored crate, verify the published archive checksum against
the registry lockfile before extracting, preserve licenses, and record the new
provenance. Review the minimal manifest or merge-write delta. Re-run clean web
builds, poisoning regressions, encoder parity and the full dependency audit.
The registry audit must include original identities for vendored Rust crates;
removing registry metadata alone is not evidence of a fix.

Remove a local override only when upstream has adopted a maintained macro or
fixed prototype handling and the same tests pass with the upstream package.
Do not rename an unmodified vulnerable package or add advisory ignore rules.
