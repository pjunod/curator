"""Tuning-only full-60 seed shallow-selection comparison.

Only Looking and Bad Buddy are scored here. Labels enter the printed metrics
after all three selection orders have been computed from provider data.
"""

import argparse
import importlib.util
import json
import math
from pathlib import Path
from seed_topic import (infer_seed_topic, summary_topic_score, detail_topic_match,
                        provider_text, summary_text, has_usable_seed_semantics, seed_token_count)


ROOT = Path(__file__).resolve().parents[2]


def module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--details", type=Path, required=True)
    parser.add_argument("--looking-pages", type=Path, required=True)
    parser.add_argument("--extra-pages", type=Path, required=True)
    parser.add_argument("--aux-pages", type=Path, required=True)
    parser.add_argument("--model-dir", type=Path, required=True)
    parser.add_argument("--score-diagnostics", action="store_true",
                        help="print tuning grades/scores after ranking; never use on held-out")
    args = parser.parse_args()
    rows = {row["id"]: row for row in json.loads(args.details.read_text())["rows"]}
    freeze = module(ROOT / "tools/curator-embed/freeze-corpus-candidates.py", "freeze_candidates")
    probe = module(ROOT / "tools/curator-embed/probe-ranking-v2.py", "probe_v2")
    old = module(ROOT / "tools/curator-embed/probe-ranking.py", "probe_v1")
    fixture = json.loads((ROOT / "docs/smart-media-discovery-m0a-revision-fixture.json").read_text())
    annotations = old.labels_from_markdown()
    for decision in json.loads((ROOT / "docs/smart-media-discovery-m0c-adjudications.json").read_text())["rows"]:
        annotations[decision["route"]][decision["id"]] = decision["adjudicated_grade"]
    looking_grades = annotations["R4_57774"] | {
        row["id"]: row["grade"] for row in json.loads((ROOT / "docs/smart-media-discovery-m0c-looking-extra-labels.json").read_text())["rows"]}
    bad_grades = {int(id): grade for id, grade in json.loads((ROOT / "docs/smart-media-discovery-m0c-seed-combined-tuning-labels.json").read_text())["queries"]["T05"]["grades"].items()}
    bad_grades |= {int(id): grade for id, grade in json.loads((ROOT / "docs/smart-media-discovery-m0c-badbuddy-extra-labels.json").read_text())["grades"].items()}
    extra_pages = {row["name"]: row for row in json.loads(args.extra_pages.read_text())["rows"]}
    looking_pages = json.loads(args.looking_pages.read_text())["summaries"]
    aux_pages = json.loads(args.aux_pages.read_text())["rows"]
    aux_manifest = {row["seed_tmdb_id"]: row for row in json.loads((ROOT / "docs/smart-media-discovery-m0c-aux-seeds.json").read_text())["queries"]}
    aux_grades = {row["seed_tmdb_id"]: {int(id):grade for id,grade in row["grades"].items()}
                  for row in json.loads((ROOT / "docs/smart-media-discovery-m0c-aux-seed-labels.json").read_text())["queries"].values()}
    assert infer_seed_topic(rows[24]) == "none"  # Real non-vocabulary, usable seed.
    assert infer_seed_topic(rows[87731]) == "none"  # Real multi-topic seed.
    assert has_usable_seed_semantics(rows[24], "none")
    assert has_usable_seed_semantics(rows[87731], "none")

    def pool(seed_id):
        if seed_id == 57774:
            pages = looking_pages
        elif seed_id == 122009:
            pages = [extra_pages[f"122009_recommendations_1"], extra_pages[f"122009_similar_1"],
                     extra_pages[f"122009_recommendations_2"]]
        else:
            pages = [next(page for page in aux_pages if page["seed_id"] == seed_id and
                     page["kind"] == kind and page["page"] == number)
                     for kind,number in (("recommendations",1),("similar",1),("recommendations",2))]
        result = []
        for id in freeze.interleave_pages(pages[:2]) + [row["id"] for row in pages[2]["results"]]:
            if id != seed_id and id not in result:
                result.append(id)
        summaries = {row["id"]: row for page in pages for row in page["results"]}
        by_list = {}
        for page in pages:
            path = page.get("path", page.get("kind", ""))
            list_name = "similar" if "similar" in path else "recommendations"
            page_number = page.get("page", page.get("params", {}).get("page", 1))
            for index, row in enumerate(page["results"]):
                by_list.setdefault(row["id"], {})[list_name] = min(
                    by_list.get(row["id"], {}).get(list_name, 10**9),
                    (page_number - 1) * 20 + index + 1)
        ranks = {id:sum(1/(60+rank) for rank in by_list[id].values()) for id in result[:60]}
        return result[:60], summaries, ranks

    seeds = [(57774, looking_grades), (122009, bad_grades),
             (655, aux_grades[655]), (1425, aux_grades[1425])]
    all_texts = []
    layout = []
    for seed_id, grades in seeds:
        ids, summaries, ranks = pool(seed_id)
        assert set(ids) == set(grades) and set(ids) <= set(rows)
        if seed_id == 57774:
            assert ids == fixture["routes"]["R4_57774"]["pool_ids"]
        topic = infer_seed_topic(rows[seed_id])
        assert has_usable_seed_semantics(rows[seed_id], topic)
        if seed_id in aux_manifest:
            assert ids == aux_manifest[seed_id]["pool_ids"]
            assert topic == aux_manifest[seed_id]["expected_seed_topic"]
        valid_ids = [id for id in ids if summary_text(summaries[id]["overview"]) is not None]
        layout.append((seed_id, ids, summaries, grades, len(all_texts), topic, ranks, valid_ids))
        all_texts.append(provider_text(rows[seed_id], topic))
        all_texts.extend(summary_text(summaries[id]["overview"]) for id in valid_ids)
    vectors, seconds = probe.encode(ROOT / "tools/curator-embed/target/release/curator-embed", args.model_dir, all_texts)
    print("seed-shallow-v1 texts", len(all_texts), "experiment_seconds", round(seconds, 3))
    for seed_id, ids, summaries, grades, offset, topic, ranks, valid_ids in layout:
        seed_vector = vectors[offset]
        candidate_vectors = vectors[offset+1:offset+1+len(valid_ids)]
        cosine = {id: sum(a*b for a,b in zip(seed_vector, vector)) for id,vector in zip(valid_ids,candidate_vectors)}
        lexical = {id: summary_topic_score(summaries[id]["overview"], topic) for id in ids}
        variants = {
            "provider30": ids[:30],
            "overview_topic30": sorted(ids, key=lambda id:(-lexical[id], ids.index(id), id))[:30],
            "semantic_shallow30": sorted(ids, key=lambda id:(-cosine.get(id,float("-inf")), ids.index(id), id))[:30],
        }
        print("seed",seed_id,"topic",topic,"seed_tokens",seed_token_count(rows[seed_id],topic),
              "pool",len(ids),"suitable_pool",sum(grades[id]>=2 for id in ids))
        for name,selected in variants.items():
            print(name,"suitable",sum(grades[id]>=2 for id in selected),
                  "top10",[(id,rows[id]["name"],grades[id]) for id in selected[:10]])
            print(name,"ids",selected)
        selected_union = list(dict.fromkeys(id for selected in variants.values() for id in selected))
        deep_texts = [provider_text(rows[seed_id], topic)] + [
            provider_text(rows[id], topic) for id in selected_union]
        deep_vectors, deep_seconds = probe.encode(
            ROOT / "tools/curator-embed/target/release/curator-embed", args.model_dir, deep_texts)
        similarity = {id:sum(a*b for a,b in zip(deep_vectors[0],vector))
                      for id,vector in zip(selected_union,deep_vectors[1:])}
        topic_match = {id:detail_topic_match(rows[id],topic) for id in selected_union}
        def report(order, selected):
            top = order[:10]
            dcg = lambda values:sum((2**v-1)/math.log2(i+2) for i,v in enumerate(values[:10]))
            full_ideal = dcg(sorted(grades.values(),reverse=True))
            selected_ideal = dcg(sorted((grades[id] for id in selected),reverse=True))
            observed = dcg([grades[id] for id in top])
            return (round(sum(grades[id]>=2 for id in top)/10,2),
                    round(observed/selected_ideal,4) if selected_ideal else None,
                    round(observed/full_ideal,4) if full_ideal else None,top)
        print("seed",seed_id,"deep_texts",len(deep_texts),"deep_experiment_seconds",round(deep_seconds,3))
        if args.score_diagnostics:
            print("score_diagnostic",seed_id,[(id,grades[id],round(similarity[id],4),topic_match[id])
                  for id in sorted(variants["semantic_shallow30"],key=lambda id:-similarity[id])])
        for name, selected in variants.items():
            orders = {
                "metadata":sorted(selected,key=lambda id:(-ranks[id],id)),
                "semantic":sorted(selected,key=lambda id:(-similarity[id],-ranks[id],id)),
                "semantic_topic_0.06":sorted(selected,key=lambda id:(-similarity[id]-0.06*topic_match[id],-ranks[id],id)),
            }
            for bonus in (0.002,0.005,0.01,0.02):
                orders[f"metadata_topic_{bonus}"] = sorted(selected,key=lambda id:(-ranks[id]-bonus*topic_match[id],id))
            orders["metadata_topic_hard"] = sorted(selected,key=lambda id:(-topic_match[id],-ranks[id],id))
            for ranker, order in orders.items():
                print("rank",name,ranker,"p10_selected_ndcg_full_ndcg_top10",report(order, selected))


if __name__ == "__main__":
    main()
