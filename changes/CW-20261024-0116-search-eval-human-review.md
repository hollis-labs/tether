- Record the human review of the search-eval labels (CW-20261024-0116). Chrispian reviewed the 25 low-confidence labels on 2026-10-30:
  23 confirmed as written, 2 widened (`t018` also accepts `schedule_create`, `t035` also accepts `caster_log`). The `review` flags are cleared,
  baselines re-recorded (BM25 recall@5 0.844, MRR 0.739; the keyword-to-BM25 gap is unchanged). Test fixture only; no behavior change.
