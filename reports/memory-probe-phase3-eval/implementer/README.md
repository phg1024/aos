# aos code's phase-3 probe samples (implementer run)

Copied from `reports/memory-probe-phase3/` in the main checkout (untracked
there), as aos code handed them over on 2026-10-29. They are the
implementer's samples: reported beside the evaluation, never pooled with it.

- Binary `.cache/bin/aos-phase3` (74510b7), probes
  `bench/memory/pathtracer-bunny-memfix-phase3-probes.json` as at 74510b7.
- 3 conditions x 5 probes x 6 samples, 2026-10-29T18:14:57Z to 20:34:00Z
  (`run.log`). Work directories were not copied (50 MB).
- sha256 prefixes: samples-memory 0dfe861df7cf, samples-no-check-rule
  ddf058cd7db6, samples-no-memory bb515dd3283a.

## Rescore with the frozen evaluation patterns (4b2514c)

`rescored-*.jsonl` come from

    aos-eval-74510b7 probe-memory --probes bench/memory/pathtracer-bunny-memfix-phase3-eval-probes.json \
        --rescore samples-C.jsonl --out rescored-C.jsonl

The five probes have the same question, topic, expect, reject, checkpoint and
working memory in both files, so the rescore changes no sample's
correct/superseded/off-topic flag. aos code's run.log scores stand as the
frozen-pattern scores:

| probe | memory | no-check-rule | no-memory |
| --- | --- | --- | --- |
| tridata-order | 6/6 | 4/6 | 2/6 (1 off-topic) |
| tg-sweep | 6/6 | 6/6 | 6/6 |
| leaf2-speedup | 6/6 | 6/6 | 5/6 |
| leaf2-readopted | 5/6 (1 superseded) | 3/6 (3 superseded) | 0/6 (4 superseded) |
| decide-leaf2-wide | 6/6 | 6/6 | 5/6 (1 superseded) |

Overlap: this run's memory condition (to 18:49Z) and no-check-rule and
no-memory conditions (18:49Z to 20:34Z) shared phg-gpu with the live run
throughout, and with the evaluation's memory condition from 19:06:50Z.
