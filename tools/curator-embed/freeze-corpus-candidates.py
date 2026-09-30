"""Freeze M0c candidate identities from captured provider result pages.

This script never reads model scores or relevance grades. The non-gay theme
routes are exploratory corpus routes, not the production retrieval contract.
"""

import argparse
import json
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def unique(ids):
    return list(dict.fromkeys(ids))


def interleave(left, right, limit):
    result = []
    for index in range(max(len(left), len(right))):
        for lane in (left, right):
            if index < len(lane) and lane[index] not in result:
                result.append(lane[index])
                if len(result) == limit:
                    return result
    return result


def interleave_pages(pages, limit=60):
    lanes = [[row["id"] for row in page["results"]] for page in pages]
    result = []
    for index in range(max(map(len, lanes))):
        for lane in lanes:
            if index < len(lane) and lane[index] not in result:
                result.append(lane[index])
                if len(result) == limit:
                    return result
    return result


def combined_paths(theme, seed):
    theme_path = theme[:30]
    seed_path = [id for id in seed if id not in set(theme_path)][:30]
    # Shared identities keep their seed rank contribution elsewhere, but the
    # theme path owns their one enrichment slot. Any spare path capacity is
    # filled from that path's remaining provider rows.
    while len(theme_path) + len(seed_path) < 60:
        added = False
        for path, source in ((theme_path, theme), (seed_path, seed)):
            for candidate_id in source:
                if candidate_id not in theme_path and candidate_id not in seed_path:
                    path.append(candidate_id)
                    added = True
                    break
            if len(theme_path) + len(seed_path) == 60:
                break
        if not added:
            break
    return theme_path, seed_path


def combined_pool(theme, seed):
    theme_path, seed_path = combined_paths(theme, seed)
    return unique(theme_path + seed_path)[:60]


def combined(theme, seed, theme_scores=None, limit=28):
    assert 1 <= limit <= 28
    theme_path, seed_path = combined_paths(theme, seed)
    merge_position = {id: index for index, id in enumerate(theme)}
    theme_scores = theme_scores or {}
    theme_path.sort(key=lambda id: (-theme_scores.get(id, 0), merge_position[id], id))
    result = unique(theme_path[:14] + seed_path[:14])
    if len(result) >= limit:
        return result[:limit]
    remaining = (theme_path[14:], seed_path[14:])
    for index in range(max(map(len, remaining))):
        for lane in remaining:
            if index < len(lane) and lane[index] not in result:
                result.append(lane[index])
                if len(result) == limit:
                    return result
    return result


def shallow_score(overview):
    text = overview.casefold()
    groups = (
        (4, r"\b(?:gay|homosexual|same.sex|boys.? love|two men|two boys|male couple|men in love)\b"),
        (2, r"\b(?:queer|lgbt|lgbtq)\b"),
        (1, r"\b(?:romance|relationship|fall in love)\b"),
    )
    return sum(weight for weight, pattern in groups if re.search(pattern, text))


def theme_pool(pages):
    order = ["gay_theme", "bl", "lgbt"]
    lanes = [pages[name]["results"] for name in order]
    pool = []
    seen = set()
    for index in range(max(map(len, lanes))):
        for lane in lanes:
            if index < len(lane) and lane[index]["id"] not in seen:
                pool.append(lane[index])
                seen.add(lane[index]["id"])
    return pool[:60]


def theme_selected(pool):
    return [row["id"] for _, row in sorted(enumerate(pool),
            key=lambda item: (-shallow_score(item[1]["overview"]), item[0], item[1]["id"]))[:30]]


def check_combined_examples():
    assert combined(list(range(1, 61)), list(range(101, 161)))[:28] == (
        list(range(1, 15)) + list(range(101, 115)))
    overlap = combined(list(range(1, 61)), list(range(1, 21)) + list(range(101, 141)))
    assert len(overlap) == 28 and len(set(overlap)) == 28
    assert overlap[:14] == list(range(1, 15))
    assert overlap[14:28] == list(range(101, 115))
    exhausted = combined([1, 2], list(range(101, 151)))
    assert len(exhausted) == 28 and exhausted[:2] == [1, 2]
    normal = combined(list(range(1, 61)), list(range(101, 161)))
    mapped = combined(list(range(1, 61)), list(range(101, 161)), limit=27)
    assert len(mapped) == 27 and mapped == normal[:27]
    assert set(normal) <= set(combined_pool(list(range(1, 61)), list(range(101, 161))))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--extra-pages", type=Path, required=True)
    parser.add_argument("--looking-pages", type=Path, required=True)
    parser.add_argument("--r3-pages", type=Path, required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "docs/smart-media-discovery-m0c-candidates-v2.json")
    args = parser.parse_args()
    check_combined_examples()

    extra = {page["name"]: page for page in json.loads(args.extra_pages.read_text())["rows"]}
    assert all(page["status"] == 200 for page in extra.values())
    looking = json.loads(args.looking_pages.read_text())["summaries"]
    assert all(page["status"] == 200 for page in looking)
    fixture = json.loads((ROOT / "docs/smart-media-discovery-m0a-revision-fixture.json").read_text())
    r3_pages = json.loads(args.r3_pages.read_text())["rows"]
    assert all(page["status"] == 200 for page in r3_pages)
    r3_by_filter = {filter_name: {page["lane"]: page for page in r3_pages if page["filter"] == filter_name}
                    for filter_name in {page["filter"] for page in r3_pages}}
    r3_pool = theme_pool(r3_by_filter["unfiltered"])
    assert [row["id"] for row in r3_pool] == fixture["routes"]["R3"]["pool_ids"]
    assert theme_selected(r3_pool) == fixture["routes"]["R3"]["preselected30_ids"]
    korean_pool = theme_pool(r3_by_filter["original_language_ko"])
    assert all(row["original_language"] == "ko" for row in korean_pool)
    queries = json.loads((ROOT / "docs/smart-media-discovery-m0c-corpus-plan.json").read_text())["queries"]

    def lane(name):
        return [row["id"] for row in extra[name]["results"]]

    def seed_pool(seed_id):
        if seed_id == 57774:
            pages = looking
        else:
            pages = [extra[f"{seed_id}_recommendations_1"], extra[f"{seed_id}_similar_1"],
                     extra[f"{seed_id}_recommendations_2"]]
        primary = interleave_pages(pages[:2])
        spill = [row["id"] for row in pages[2]["results"]]
        return [id for id in unique(primary + spill) if id != seed_id][:60]

    theme = {
        "gay_male": theme_selected(r3_pool),
        "lgbtq": lane("lgbt"),
        "lesbian": interleave(lane("lesbian"), lane("lesbian_romance"), 30),
        "coming_of_age": lane("coming_of_age"),
        "found_family": interleave(lane("found_family"), lane("chosen_family"), 30),
        "political_drama": interleave(lane("political_drama"), lane("politics"), 30),
        "space_exploration": interleave(lane("space_exploration"), lane("space_travel"), 30),
    }
    seeds = {seed: seed_pool(seed) for seed in (57774, 122009, 18745, 87731)}
    assert seeds[57774] == fixture["routes"]["R4_57774"]["pool_ids"]

    captured = []
    for query in queries:
        mode = query["mode"]
        if mode == "theme":
            candidates = theme_selected(korean_pool) if query["id"] == "H02" else theme[query["theme"]]
        elif mode == "seed":
            candidates = seeds[query["seed_tmdb_id"]][:30]
        elif mode == "combined":
            candidates = combined([row["id"] for row in r3_pool], seeds[query["seed_tmdb_id"]],
                                  {row["id"]: shallow_score(row["overview"]) for row in r3_pool})
        else:
            candidates = []
        route_status = ("normative_r3" if query.get("theme") == "gay_male" else
                        "normative_seed" if mode == "seed" and query.get("seed_tmdb_id") == 57774 else
                        "exploratory")
        captured.append({"query_id": query["id"], "mode": mode, "candidate_ids": candidates,
                         "candidate_count": len(candidates), "route_status": route_status})
    ids = {id for query in captured for id in query["candidate_ids"]}
    tuning = {id for query in captured if query["query_id"].startswith("T") for id in query["candidate_ids"]}
    held = {id for query in captured if query["query_id"].startswith("H") for id in query["candidate_ids"]}
    output = {
        "version": "m0c-candidate-freeze-v2",
        "captured_local_date": "2026-09-29",
        "source": "fixed R3 revision fixture; captured unfiltered and Korean-filtered R3 summary pages; other captured TMDB list pages in private temporary files",
        "status": "candidate identities only; relevance labels and scoring not frozen",
        "selection": "R3 three-lane merge and lexical summary score; Korean query repeats all three lanes with upstream with_original_language=ko; seed recommendations/similar first-page absolute-rank merge then page-2 spill; combined 30/30 disjoint path quota, first14 each, alternating fill to28; exploratory other-theme first20 or alternating two exact-keyword lanes up to30",
        "distinct_candidates": len(ids),
        "distinct_tuning_candidates": len(tuning),
        "distinct_heldout_candidates": len(held),
        "heldout_tuning_identity_overlap": len(held & tuning),
        "heldout_new_identities": len(held - tuning),
        "queries": captured,
    }
    args.output.write_text(json.dumps(output, indent=2) + "\n")
    print("distinct", len(ids), "tuning", len(tuning), "heldout", len(held),
          "heldout overlap", len(held & tuning), "heldout new", len(held - tuning))


if __name__ == "__main__":
    main()
