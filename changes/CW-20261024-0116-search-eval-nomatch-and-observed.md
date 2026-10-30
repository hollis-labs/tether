- Extend the search-eval fixture (CW-20261024-0116). It gains 8 "no tool should match" queries with a false-positive measure
  (the current ranking returns tools for 5 of 8; recorded in the baseline), 3 queries taken from real agent searches, and
  24 held-out queries. Task-derived queries could not be built: only 2 tasks have sessions with tool-call
  events and only about 8 genuine agent searches exist (the rest of the telemetry is test traffic). The 22 AI-flagged labels still need a
  human; a review sheet was prepared.
