# Blind grades, key and agreement with the regex scorer

Grades: aos book session, 2026-10-29, given before the key was released
(`key.json`, SHA-256 a5d4ba71f25f7caf08426c66905428418d3364cad6f67bab493c9291ee9b326e,
as stated in `blind-answers.md` at 35ca544).

For agreement, a reader's "correct" counts as correct and "partly
correct", "wrong" and "no answer" count as not correct. That is the
regex scorer's standard: leaf2-speedup needs the bug as the reason.

| item | phase | probe | source | regex score | reader verdict | reader: superseded | reader: invented | agree |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 1 | tridata-order | archive-x6/samples-phase1 #3 | correct | correct | no | none | yes |
| 2 | 1 | leaf2-speedup | archive-x6/samples-phase1 #6 | not correct | partly (right verdict, wrong reason) | yes | material | yes |
| 3 | 3 | leaf2-speedup | memory-probe-phase3/samples-memory #6 (aos code's run) | correct | correct | no | none | yes |
| 4 | 3 | tg-sweep | memory-probe-phase3/samples-memory #6 (aos code's run) | correct | partly ("4 threads per threadgroup"; TG4 is 4x4 = 16) | yes | minor | **no** |
| 5 | 2 (ecb29d4) | tridata-order | archive-x6/samples-phase2 #3 | not correct | wrong ("never measured") | yes | none | yes |
| 6 | 2 (ff46bbc) | leaf2-speedup | precedence/samples-rescored #7 | off-topic | no answer | no | none | yes |
| 7 | 2 (ecb29d4) | tg-sweep | samples-rescored #3 | correct | correct | no | none | yes |
| 8 | 2 (ff46bbc) | leaf2-speedup | precedence/samples-rescored #4 | correct | correct | no | minor (0.475902 was the fixed leaf2, not the reverted build) | yes |
| 9 | 3 | tridata-order | memory-probe-phase3/samples-memory #3 (aos code's run) | correct | correct | no | minor (invented past rationale on tie behaviour) | yes |
| 10 | 1 | tg-sweep | baseline/samples-rescored #1 | correct | correct | no | none | yes |

**Verdict agreement: 9 of 10.** The disagreement is item 4. The tg-sweep
pattern accepts any "TG4" and does not check what TG4 means. The reader
found the answer wrong about the thread count. The `superseded` flag of
the regex scorer fired on none of the ten; the reader marked three (items
2, 4, 5). Inventions are invisible to the regex scorer by construction:
the reader found one material (item 2, already not correct) and three
minor (items 4, 8, 9).

On the items the book session singled out:
- Item 2: both say not correct, for the same reason (verdict right, reason
  wrong). The regex scorer only knows that the bug is missing; the reader
  also found an invented claim ("it never was" re-checked).
- Item 8: both say correct. The reader's misattributed number (0.475902)
  does not change the verdict, and a regex cannot see it.
- Item 9: both say correct. The invented rationale is beyond the regex.

Two of the four inventions and one of the three superseded statements
are in the three phase-3 answers (items 4 and 9). Item 4's error looks
like it comes from the terse working-memory entry "TG4 kept: interleaved
TG4 45.3 vs TG2 47.0 ms", which does not say TG4 is a 4x4 tile. That is
one sample and is not evidence of a rate. It is a reason to check
working-memory-sourced answers for paraphrase errors in the full phase-3
evaluation.

No pattern is changed because of this. The frozen tg-sweep pattern stays,
and the phase-3 report gives hand-read verdicts beside the regex ones.
