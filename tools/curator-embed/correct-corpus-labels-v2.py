"""Reconcile v1 agent labels with corrected v2 candidate IDs, pre-ranking.

The resulting v2 file remains historical if seed preselection changes again.
No model scores, ranking positions or held-out result are read.
"""

import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def load(name):
    return json.loads((ROOT / "docs" / name).read_text())


def main():
    old = {row["query_id"]: {int(id): grade for id,grade in row["grades"].items()}
           for row in load("smart-media-discovery-m0c-labels.json")["queries"]}
    queries = {row["query_id"]: row for row in load("smart-media-discovery-m0c-candidates-v2.json")["queries"]}
    origin = {row["tmdb_id"]: row["origin_country"] for row in load("smart-media-discovery-m0c-r3-origins.json")["rows"]}
    asian_origins = {"TH", "JP", "KR", "CN", "TW", "HK", "PH", "SG", "MY", "ID", "VN", "IN", "PK", "BD", "NP", "LK", "MM", "KH", "LA", "MN"}
    old["H01"] = {id: old["T01"][id] if asian_origins.intersection(origin[id]) else 0
                  for id in queries["H01"]["candidate_ids"]}
    korean = {int(id):grade for id,grade in load("smart-media-discovery-m0c-korean-labels.json")["grades"].items()}
    old["H02"] = korean
    old["T03"] = {id:old["T03"].get(id,{1413:0,87731:2}[id] if id in (1413,87731) else None)
                  for id in queries["T03"]["candidate_ids"]}
    old["T06"] = {id:old["T06"].get(id,2 if id==97186 else None)
                  for id in queries["T06"]["candidate_ids"]}
    teen_focus = {127549,67429,76669,124834,69050,81356,88040,117581,
                  97186,90462,125910,62914,75208,278196,195670,102903}
    old["H03"] = {id:0 if id in teen_focus else old["T03"][id]
                  for id in queries["H03"]["candidate_ids"]}
    # Found family and chosen family are near-synonyms in this rubric. The
    # query-level split is retained, without an unexplained grade shift.
    old["H09"] = dict(old["T10"])
    result = []
    for query_id, query in queries.items():
        if query_id == "H12":
            continue
        ids = query["candidate_ids"]
        grades = old[query_id]
        assert set(grades) == set(ids), (query_id,set(ids)-set(grades),set(grades)-set(ids))
        assert all(grade in (0,1,2,3) for grade in grades.values())
        result.append({"query_id":query_id,"grades":{str(id):grades[id] for id in sorted(ids)},
                       "judged_count":len(ids),"suitable_count":sum(grades[id]>=2 for id in ids)})
    output={"version":"m0c-provisional-label-freeze-v2",
            "status":"agent-assigned/derived labels corrected for v2 candidates before held-out model order; not final policy or independent editorial truth",
            "source":"v1 agent labels with ID-specific combined additions, Korean-filtered provider facts, and TMDB production-country H01 rubric",
            "h01_rubric":"Asian-produced TV means a TMDB origin_country in the recorded Asian ISO country set; setting alone does not qualify; this is evaluation preference, not a supported v1 runtime hard filter",
            "h09_rubric":"chosen family and found family treated as equivalent absent query-specific evidence",
            "queries":result,
            "refinement":{"query_id":"H12","expected":"needs_refinement","ranked_candidates":0}}
    path=ROOT / "docs/smart-media-discovery-m0c-labels-v2.json"
    path.write_text(json.dumps(output,indent=2)+"\n")
    print("ranked queries",len(result),"judgments",sum(q["judged_count"] for q in result),
          "distinct",len({int(id) for q in result for id in q["grades"]}))


if __name__ == "__main__":
    main()
