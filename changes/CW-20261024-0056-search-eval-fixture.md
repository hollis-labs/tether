- Add a tool-discovery eval (CW-20261024-0056): `make search-eval` scores the gateway
  scorer against a pinned 725-tool catalog snapshot and 138 labeled intent queries
  (head, tail, hard) and prints recall@1/5/10, MRR and median response bytes against a
  committed baseline. The harness takes the scorer as a parameter so the BM25 change
  and the code-mode experiment can be judged by the same numbers. Test data only; no
  runtime change.
