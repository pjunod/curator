"""Freeze query-specific provisional agent-assigned labels before held-out scoring.

No encoder, model score, or ranking output is read. The generated judgments
are still provisional where provider descriptions are thin; the reviewer must
adjudicate consequential disputes before treating metrics as a final gate.
"""

import argparse
import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def load(name):
    return json.loads((ROOT / "docs" / name).read_text())


def ints(mapping):
    return {int(key): value for key, value in mapping.items()}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--details", type=Path, required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "docs/smart-media-discovery-m0c-labels.json")
    args = parser.parse_args()
    details = {row["id"]: row for row in json.loads(args.details.read_text())["rows"]}
    candidates = {row["query_id"]: row["candidate_ids"] for row in load("smart-media-discovery-m0c-candidates.json")["queries"]}
    queries = {row["id"]: row for row in load("smart-media-discovery-m0c-corpus-plan.json")["queries"]}
    other = load("smart-media-discovery-m0c-other-theme-tuning-labels.json")["queries"]
    combined = load("smart-media-discovery-m0c-seed-combined-tuning-labels.json")["queries"]
    import importlib.util
    old_path = ROOT / "tools/curator-embed/probe-ranking.py"
    spec = importlib.util.spec_from_file_location("probe_ranking_v1", old_path)
    old = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(old)
    originals = old.labels_from_markdown()
    adjudications = load("smart-media-discovery-m0c-adjudications.json")["rows"]
    for decision in adjudications:
        assert originals[decision["route"]][decision["id"]] == decision["original_grade"]
        originals[decision["route"]][decision["id"]] = decision["adjudicated_grade"]
    extras = load("smart-media-discovery-m0c-looking-extra-labels.json")["rows"]
    looking = originals["R4_57774"] | {row["id"]: row["grade"] for row in extras}

    grades = {
        "T01": {id: originals["R3"][id] for id in candidates["T01"]},
        "T02": {id: looking[id] for id in candidates["T02"]},
    }
    for source in (other, combined):
        for query_id, row in source.items():
            grades[query_id] = ints(row["grades"])

    # The exact teen-focus list is an independent query constraint, not a
    # relevance grade from an embedding rank. Adult university BL stays in.
    teen_theme = {127549, 67429, 76669, 124834, 69050, 81356, 88040,
                  117581, 97186, 90462, 125910, 62914, 75208, 278196,
                  195670, 102903}
    grades["T04"] = {id: 0 if id in teen_theme else grades["T01"][id]
                     for id in candidates["T04"]}

    asian_languages = {"th", "ja", "ko", "zh", "tl", "id", "vi", "ms", "hi"}
    grades["H01"] = {id: grades["T01"][id] if details[id].get("original_language") in asian_languages else 0
                     for id in candidates["H01"]}
    grades["H02"] = {id: grades["T01"][id] if details[id].get("original_language") == "ko" else 0
                     for id in candidates["H02"]}
    teen_combined = teen_theme
    grades["H03"] = {id: 0 if id in teen_combined else grades["T03"][id]
                     for id in candidates["H03"]}
    teen_broad = {69050, 76669, 81356, 90462, 102903, 100883, 93741,
                  92685, 124834, 97186}
    grades["H06"] = {id: 0 if id in teen_broad else grades["T07"][id]
                     for id in candidates["H06"]}
    grades["H07"] = grades["T08"] | {id: grade for id, grade in {
        106431: 1, 2603: 1, 132009: 0, 248065: 0, 53128: 0,
        94605: 2, 283052: 3,
    }.items()}
    grades["H08"] = {
        66732:0, 85552:0, 81356:0, 85937:0, 87739:2, 70785:0,
        99966:0, 74577:0, 40075:0, 71728:0, 33880:0, 46298:0,
        90260:0, 100883:0, 76121:0, 124834:0, 97186:0, 92782:0,
        89905:2, 61175:0,
    }
    grades["H09"] = grades["T10"] | {id: grade for id, grade in {
        70785:3, 136315:2, 120089:3, 262388:0, 76121:1, 304078:0,
        137040:0, 212989:2, 237565:3, 65890:1,
    }.items()}
    grades["H10"] = grades["T11"] | {id: grade for id, grade in {
        2947:1, 8592:0, 1429:2, 31911:1, 225399:2, 61223:2,
        87917:0, 31724:2,
    }.items()}
    grades["H11"] = dict(grades["T12"])
    grades["H04"] = {
        87731:3, 65230:0, 123192:1, 313899:0, 223313:0, 6539:1,
        99617:2, 132925:0, 130464:0, 132959:0, 244244:0, 65294:0,
        210787:1, 214081:0, 118958:1, 6362:0, 109958:1, 6395:0,
        203744:0, 65116:0, 129612:0, 65134:0, 88324:1, 65153:0,
        233256:0, 213951:2, 110531:1, 313774:0, 284605:0, 65177:0,
    }
    grades["H05"] = {
        18745:3, 5919:0, 123192:1, 5960:0, 199001:0, 97952:2,
        118958:1, 64817:0, 250988:1, 132198:0, 1365:1, 64835:2,
        46441:0, 208533:2, 130464:0, 240440:0, 3475:2, 240445:0,
        107365:0, 240448:0, 210787:1, 208591:1, 80006:2, 208594:2,
        89630:2, 873:0, 203744:0, 93046:0, 130842:2, 890:0,
    }
    assert set(grades) == set(candidates) - {"H12"}
    for query_id, query_grades in grades.items():
        assert set(query_grades) == set(candidates[query_id]), (query_id, set(candidates[query_id]) - set(query_grades))
        assert all(grade in (0, 1, 2, 3) for grade in query_grades.values())
    output = {
        "version": "m0c-provisional-label-freeze-v1",
        "frozen_local_date": "2026-09-29",
        "status": "provisional agent-assigned or derived labels frozen before any held-out embedding order; independent editorial adjudication pending for consequential disputes",
        "source": "agent judgments from TMDB facts and prior M0a annotation; four tuning cases independently adjudicated by reviewer agent; some held-out labels algorithmically transformed from tuning labels",
        "grade_rule": "0 unrelated, 1 weak/incidental, 2 substantial, 3 central full-query fit; 2+ suitable",
        "queries": [{"query_id": query_id, "prose": queries[query_id]["prose"],
                     "grades": {str(id): grade for id, grade in sorted(grades[query_id].items())},
                     "judged_count": len(grades[query_id]),
                     "suitable_count": sum(grade >= 2 for grade in grades[query_id].values())}
                    for query_id in candidates if query_id in grades],
        "refinement": {"query_id": "H12", "expected": "needs_refinement", "ranked_candidates": 0},
    }
    args.output.write_text(json.dumps(output, indent=2) + "\n")
    print("ranked queries", len(grades), "judgments", sum(len(v) for v in grades.values()),
          "distinct judged identities", len({id for v in grades.values() for id in v}))


if __name__ == "__main__":
    main()
