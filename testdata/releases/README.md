# testdata/releases — golden parser corpus (Phase 2)

The single most important test asset in the project (blueprint §8): release-name test cases
ported wholesale from the Sonarr and Radarr parser test suites (GPL-3.0, same license — see
ADR 0001), stored as JSON with upstream's expected outputs treated as the spec.

Format (one file per category, e.g. `scene.json`, `anime.json`, `daily.json`, `editions.json`):

```json
[
  {
    "title": "Show.Name.S01E01E02.1080p.WEB-DL.DD5.1.H.264-GROUP",
    "expect": { "series": "Show Name", "season": 1, "episodes": [1, 2],
                "resolution": "1080p", "source": "webdl", "group": "GROUP" }
  }
]
```

The parser conformance test reports a percentage against this corpus in CI; fuzzing runs on
top (the parser must never panic on arbitrary bytes).
