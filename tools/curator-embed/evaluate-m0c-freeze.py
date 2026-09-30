"""Run the frozen M0c policy once against captured provider data.

This is an offline prototype evaluator, not the Curator production route.
Labels are validated before any model call and only enter metric calculation.
"""

import argparse
import importlib.util
import json
import math
import re
import subprocess
from pathlib import Path

from seed_topic import (detail_topic_match, has_usable_seed_semantics,
                        infer_seed_topic, provider_text, summary_text)

ROOT = Path(__file__).resolve().parents[2]
TEEN_KEYWORDS = {"teen drama", "teenager", "teenagers", "high school",
                 "high school student", "high school students", "school romance",
                 "school life", "lgbt teen", "teen coming of age"}
TEEN_OVERVIEW = re.compile(
    r"\b(?:a|the|an?) (?:teen(?:age)?|high.school) (?:boy|girl|student|pupil|protagonist)\b"
    r"|\b(?:teen(?:age)?|high.school) (?:boy|girl|student|pupil)\b", re.I)
SEED_CUTOFF = 0.48
LESBIAN = re.compile(r"\b(?:lesbian|girls.? love|women in love|romance between women)\b", re.I)


def module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


def data(path):
    return json.loads(path.read_text())


def dcg(grades):
    return sum((2 ** grade - 1) / math.log2(index + 2)
               for index, grade in enumerate(grades[:10]))


def metrics(order, grades, selected, full):
    order = order[:20]
    observed = dcg([grades[id] for id in order])
    selected_ideal = dcg(sorted((grades[id] for id in selected), reverse=True))
    full_ideal = dcg(sorted((grades[id] for id in full), reverse=True))
    return {
        "returned": len(order),
        "suitable_at_10": sum(grades[id] >= 2 for id in order[:10]),
        "p_at_10": round(sum(grades[id] >= 2 for id in order[:10]) / 10, 4)
        if len(order) >= 10 else None,
        "ndcg_selected": round(observed / selected_ideal, 4) if selected_ideal else None,
        "ndcg_full": round(observed / full_ideal, 4) if full_ideal else None,
        "top10_ids": order[:10],
    }


def teen_focus(row):
    return bool({word.casefold() for word in row["keywords"]} & TEEN_KEYWORDS
                or TEEN_OVERVIEW.search(row["overview"]))


def exploratory_theme_match(row, theme):
    if theme == "lgbtq":
        return bool(detail_topic_match(row, "queer"))
    if theme == "lesbian":
        return bool(LESBIAN.search(" ".join(row["keywords"]) + " " + row["overview"]))
    return bool(detail_topic_match(row, theme))


def required_theme_tier(row, theme, ranking):
    return ranking.evidence_tier(row) if theme == "gay_male" else int(
        exploratory_theme_match(row, theme))


def required_theme_admitted(row, theme, ranking):
    return required_theme_tier(row, theme, ranking) > 0


def list_rrf(pages, ids):
    positions = {}
    for lane, page in pages:
        number = page.get("page", page.get("params", {}).get("page", 1))
        for index, row in enumerate(page["results"]):
            positions.setdefault(row["id"], {})[lane] = min(
                positions.get(row["id"], {}).get(lane, 10 ** 9),
                (number - 1) * 20 + index + 1)
    return {id: sum(1 / (60 + rank) for rank in positions[id].values()) for id in ids}


def seed_pages(seed, extra, looking):
    if seed == 57774:
        return looking
    return [extra[f"{seed}_recommendations_1"],
            extra[f"{seed}_similar_1"],
            extra[f"{seed}_recommendations_2"]]


def seed_pool(seed, pages, freeze):
    ids = freeze.interleave_pages(pages[:2]) + [row["id"] for row in pages[2]["results"]]
    return [id for id in dict.fromkeys(ids) if id != seed][:60]


def seed_rrf(pages, ids):
    positions = {}
    for page in pages:
        path = page.get("path", page.get("name", ""))
        lane = "similar" if "similar" in path else "recommendations"
        number = page.get("page", page.get("params", {}).get("page", 1))
        for index, row in enumerate(page["results"]):
            positions.setdefault(row["id"], {})[lane] = min(
                positions.get(row["id"], {}).get(lane, 10 ** 9),
                (number - 1) * 20 + index + 1)
    return {id: sum(1 / (60 + rank) for rank in positions[id].values()) for id in ids}


def dot(left, right):
    return sum(a * b for a, b in zip(left, right))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--details", type=Path, required=True)
    parser.add_argument("--looking-pages", type=Path, required=True)
    parser.add_argument("--extra-pages", type=Path, required=True)
    parser.add_argument("--r3-pages", type=Path, required=True)
    parser.add_argument("--model-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args()
    rows = {row["id"]: row for row in data(args.details)["rows"]}
    extra = {page["name"]: page for page in data(args.extra_pages)["rows"]}
    looking = data(args.looking_pages)["summaries"]
    r3_rows = data(args.r3_pages)["rows"]
    plan = {query["id"]: query for query in data(ROOT / "docs/smart-media-discovery-m0c-corpus-plan.json")["queries"]}
    qtext = {row["query_id"]: row for row in
             data(ROOT / "docs/smart-media-discovery-m0c-qtext-fixtures.json")["rows"]}
    for qid, row in qtext.items():
        assert row["source_prose"] == plan[qid]["prose"]
        assert row["filters"] == plan[qid]["filters"]
        assert row["theme"] == plan[qid]["theme"]
    assert set(qtext) == {qid for qid, query in plan.items()
                          if query["mode"] in ("theme", "combined")}
    snapshots = {query["query_id"]: query for query in data(ROOT / "docs/smart-media-discovery-m0c-candidates-v2.json")["queries"]}
    labels = {query["query_id"]: {int(id): grade for id, grade in query["grades"].items()}
              for query in data(ROOT / "docs/smart-media-discovery-m0c-labels-v2.json")["queries"]}
    tales = data(ROOT / "docs/smart-media-discovery-m0c-tales-extra-labels.json")["queries"]
    for qid in ("H04", "H05"):
        labels[qid].update({int(id): grade for id, grade in tales[qid]["grades"].items()})
    extras = data(ROOT / "docs/smart-media-discovery-m0c-looking-extra-labels.json")["rows"]
    labels["T02"].update({row["id"]: row["grade"] for row in extras})
    old = module(ROOT / "tools/curator-embed/probe-ranking.py", "old_rank")
    labels["T02"].update(old.labels_from_markdown()["R4_57774"])
    for decision in data(ROOT / "docs/smart-media-discovery-m0c-adjudications.json")["rows"]:
        if decision["route"] == "R4_57774":
            labels["T02"][decision["id"]] = decision["adjudicated_grade"]
    labels["T05"].update({int(id): grade for id, grade in
                           data(ROOT / "docs/smart-media-discovery-m0c-badbuddy-extra-labels.json")["grades"].items()})
    freeze = module(ROOT / "tools/curator-embed/freeze-corpus-candidates.py", "freeze")
    ranking = module(ROOT / "tools/curator-embed/probe-ranking-v2.py", "rank")
    ranking.check_evidence_examples()
    wrong_theme = {"keywords": ["male friendship", "friendship"],
                   "overview": "Three men develop a close friendship in San Francisco."}
    assert not required_theme_admitted(wrong_theme, "gay_male", ranking)
    seeds = {}
    for seed in (57774, 122009, 18745, 87731):
        pages = seed_pages(seed, extra, looking)
        pool = seed_pool(seed, pages, freeze)
        summaries = {item["id"]: item for page in pages for item in page["results"]}
        topic = infer_seed_topic(rows[seed])
        seeds[seed] = {"pool": pool, "summaries": summaries,
                       "topic": topic, "rrf": seed_rrf(pages, pool)}
    r3 = {}
    for filter_name in ("unfiltered", "original_language_ko"):
        lanes = [(lane, next(page for page in r3_rows if page["filter"] == filter_name
                            and page["lane"] == lane)) for lane in ("gay_theme", "bl", "lgbt")]
        pool = freeze.theme_pool(dict(lanes))
        ids = [row["id"] for row in pool]
        r3[filter_name] = {"pool": ids, "rrf": list_rrf(lanes, ids)}
    theme_lanes = {
        "lgbtq": ("lgbt",), "lesbian": ("lesbian", "lesbian_romance"),
        "coming_of_age": ("coming_of_age",),
        "found_family": ("found_family", "chosen_family"),
        "political_drama": ("political_drama", "politics"),
        "space_exploration": ("space_exploration", "space_travel"),
    }
    other_theme = {}
    for theme, lanes in theme_lanes.items():
        pages = [(lane, extra[lane]) for lane in lanes]
        pool = freeze.interleave_pages([page for _, page in pages], limit=60)
        other_theme[theme] = {"pool": pool, "rrf": list_rrf(pages, pool)}
    for qid, seed in (("T02", 57774), ("T05", 122009),
                      ("H04", 18745), ("H05", 87731)):
        assert set(labels[qid]) == set(seeds[seed]["pool"]), qid
    assert all(row["status"] == 200 for row in rows.values())
    for qid, snap in snapshots.items():
        if qid == "H12":
            continue
        assert set(snap["candidate_ids"]) <= set(rows), qid
        query = plan[qid]
        if query["mode"] == "theme":
            source = (r3["original_language_ko" if qid == "H02" else "unfiltered"]
                      if query["theme"] == "gay_male" else other_theme[query["theme"]])
            assert set(snap["candidate_ids"]) <= set(source["pool"]), qid
            assert set(snap["candidate_ids"]) <= set(source["rrf"]), qid
        if query["mode"] == "combined":
            assert set(snap["candidate_ids"]) <= (
                set(r3["unfiltered"]["pool"]) | set(seeds[query["seed_tmdb_id"]]["pool"])), qid
        if plan[qid]["mode"] != "seed":
            assert set(snap["candidate_ids"]) == set(labels[qid]), qid

    # Build every text and validate the entire layout before first model call.
    texts = []
    text_index = {}
    def add(key, value):
        if key not in text_index:
            assert value is not None and len(value.encode("utf-8")) <= 8192
            text_index[key] = len(texts)
            texts.append(value)
    for qid, query in plan.items():
        if query["mode"] in ("theme", "combined"):
            add(("theme", qid), qtext[qid]["normalized_qtext"])
    for seed, info in seeds.items():
        add(("seed", seed), provider_text(rows[seed], info["topic"]))
        if has_usable_seed_semantics(rows[seed], info["topic"]):
            for id in info["pool"]:
                summary = summary_text(info["summaries"][id]["overview"])
                if summary is not None:
                    add(("summary", seed, id), summary)
    for qid, snap in snapshots.items():
        if qid == "H12":
            continue
        query = plan[qid]
        topic = seeds[query["seed_tmdb_id"]]["topic"] if query["mode"] in ("seed", "combined") else "none"
        ids = seeds[query["seed_tmdb_id"]]["pool"] if query["mode"] == "seed" else snap["candidate_ids"]
        for id in ids:
            assert len(provider_text(rows[id], topic).encode("utf-8")) <= 8192
    if args.validate_only:
        print("preflight ok", len(rows), "details;", len(texts), "first-phase texts")
        return
    model_dir = args.model_dir
    helper = ROOT / "tools/curator-embed/target/release/curator-embed"
    shallow_vectors, shallow_seconds = ranking.encode(helper, model_dir, texts)
    shallow_vector = lambda key: shallow_vectors[text_index[key]]
    selected_by_seed = {}
    for seed, info in seeds.items():
        pool = info["pool"]
        if has_usable_seed_semantics(rows[seed], info["topic"]):
            shallow = {id: dot(shallow_vector(("seed", seed)),
                               shallow_vector(("summary", seed, id)))
                       for id in pool if ("summary", seed, id) in text_index}
            selected = sorted(pool, key=lambda id: (-shallow.get(id, float("-inf")),
                                                   pool.index(id), id))[:30]
        else:
            selected = pool[:30]
        selected_by_seed[seed] = selected
    detail_texts = []
    detail_index = {}
    for qid, snap in snapshots.items():
        if qid == "H12":
            continue
        query = plan[qid]
        topic = seeds[query["seed_tmdb_id"]]["topic"] if query["mode"] in ("seed", "combined") else "none"
        ids = selected_by_seed[query["seed_tmdb_id"]] if query["mode"] == "seed" else snap["candidate_ids"]
        for id in ids:
            key = ("detail", id, topic)
            if key not in detail_index:
                detail_index[key] = len(detail_texts)
                detail_texts.append(provider_text(rows[id], topic))
    detail_vectors, detail_seconds = ranking.encode(helper, model_dir, detail_texts)
    vector = lambda key: (detail_vectors[detail_index[key]] if key[0] == "detail"
                          else shallow_vector(key))
    revision = subprocess.check_output(["git", "rev-parse", "--short", "HEAD"],
                                       cwd=ROOT, text=True).strip()
    output = {"version": "m0c-heldout-repaired-evaluation-v3", "policy_commit": revision,
              "first_run_status": "supersedes invalid first measurement; same consumed queries",
              "label_provenance": "agent-assigned/derived; four reviewer-agent adjudications",
              "shallow_texts": len(texts), "detail_texts": len(detail_texts),
              "encoder_seconds": round(shallow_seconds + detail_seconds, 3),
              "queries": []}
    for qid in [query["id"] for query in plan.values()]:
        query = plan[qid]
        if qid == "H12":
            output["queries"].append({"query_id": qid, "status": "needs_refinement",
                                      "pool": 0, "selected": 0, "eligible": 0, "returned": 0})
            continue
        mode, snap = query["mode"], snapshots[qid]
        if mode == "seed":
            seed = query["seed_tmdb_id"]
            info = seeds[seed]
            pool = info["pool"]
            seed_vector = vector(("seed", seed))
            selected = selected_by_seed[seed]
            score = {id: dot(seed_vector, vector(("detail", id, info["topic"])))
                     for id in selected}
            eligible = [id for id in selected if score[id] >= SEED_CUTOFF]
            rank = info["rrf"]
            meta_order = sorted(eligible, key=lambda id: (-rank[id] - 0.02 * detail_topic_match(rows[id], info["topic"]), id))
            model_order = sorted(eligible, key=lambda id: (-score[id] - 0.06 * detail_topic_match(rows[id], info["topic"]), -rank[id], id))
            off_selected = pool[:30]
            off_eligible = off_selected
            off_order = sorted(off_eligible, key=lambda id: (-rank[id] - 0.02 * detail_topic_match(rows[id], info["topic"]), id))
            full = pool
            full_scope = "graded_full_related_pool"
        else:
            selected = snap["candidate_ids"]
            if query["theme"] == "gay_male":
                theme_info = r3["original_language_ko" if qid == "H02" else "unfiltered"]
            else:
                theme_info = other_theme[query["theme"]]
            pool = theme_info["pool"]
            if mode == "combined":
                pool = freeze.combined_pool(pool, seeds[query["seed_tmdb_id"]]["pool"])
                assert set(selected) <= set(pool), qid
            full = selected
            full_scope = "graded_selected_only"
            theme = query["theme"]
            theme_vector = vector(("theme", qid))
            if mode == "combined":
                seed = query["seed_tmdb_id"]
                info = seeds[seed]
                seed_vector = vector(("seed", seed))
            else:
                info = None
            def tier(id):
                return required_theme_tier(rows[id], theme, ranking) if theme == "gay_male" else 0
            def admitted(id):
                return required_theme_admitted(rows[id], theme, ranking)
            missing_required_detail = [id for id in selected
                                       if query["filters"].get("original_language")
                                       and not rows[id].get("original_language")]
            eligible = [id for id in selected if admitted(id)
                        and (not query["filters"].get("original_language")
                             or rows[id].get("original_language") == query["filters"]["original_language"])
                        and (not query["filters"].get("excludeTeenFocus") or not teen_focus(rows[id]))]
            def theme_score(id):
                return dot(theme_vector, vector(("detail", id, info["topic"] if info else "none")))
            if mode == "combined":
                def seed_score(id):
                    return dot(seed_vector, vector(("detail", id, info["topic"])))
                rank_theme = theme_info["rrf"]
                rank_seed = info["rrf"]
                meta_order = sorted(eligible, key=lambda id: (-tier(id),
                    -(0.6 * rank_theme.get(id, 0) + 0.4 * rank_seed.get(id, 0)
                      + 0.02 * detail_topic_match(rows[id], info["topic"])), id))
                model_order = sorted(eligible, key=lambda id: (-tier(id),
                    -(0.6 * theme_score(id) + 0.4 * seed_score(id)
                      + 0.06 * detail_topic_match(rows[id], info["topic"])), id))
            else:
                meta_order = sorted(eligible, key=lambda id: (-tier(id), -theme_info["rrf"][id], id))
                model_order = sorted(eligible, key=lambda id: (-tier(id), -theme_score(id),
                                                             -theme_info["rrf"][id], id))
            off_selected = selected
            off_eligible = eligible
            off_order = meta_order
        grades = labels[qid]
        assert set(selected) <= set(grades), qid
        assert set(full) <= set(grades), qid
        assert set(off_selected) <= set(grades), qid
        output["queries"].append({
            "query_id": qid, "split": query["split"], "mode": mode,
            "route_status": snap["route_status"],
            "pool": len(pool),
            "pool_suitable": sum(grades[id] >= 2 for id in pool) if set(pool) <= set(grades) else None,
            "full_ideal_scope": full_scope,
            "selected": len(selected), "selected_suitable": sum(grades[id] >= 2 for id in selected),
            "eligible": len(eligible), "eligible_suitable": sum(grades[id] >= 2 for id in eligible),
            "missing_required_detail_ids": missing_required_detail if mode != "seed" else [],
            "returned": min(20, len(model_order)),
            "model_off_selected": len(off_selected),
            "model_off_eligible": len(off_eligible),
            "model_off_returned": min(20, len(off_order)),
            "model_off": metrics(off_order, grades, off_selected, full),
            "same_pool_metadata": metrics(meta_order, grades, selected, full),
            "same_pool_model": metrics(model_order, grades, selected, full),
            "model_off_order_ids": off_order[:20],
            "same_pool_metadata_order_ids": meta_order[:20],
            "same_pool_model_order_ids": model_order[:20],
            "selected_ids": selected,
        })
    args.output.write_text(json.dumps(output, indent=2) + "\n")
    print("encoded", len(texts), "shallow and", len(detail_texts), "detail texts; seconds",
          round(shallow_seconds + detail_seconds, 3))
    for row in output["queries"]:
        if row["query_id"] == "H12":
            continue
        print(row["query_id"], row["pool"], row["selected"], row["eligible"],
              row["same_pool_metadata"]["p_at_10"], row["same_pool_model"]["p_at_10"],
              row["same_pool_metadata"]["ndcg_full"], row["same_pool_model"]["ndcg_full"])


if __name__ == "__main__":
    main()
