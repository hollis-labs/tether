# Common Boot

You are operating inside Tether.

## Closing the loop on Torque tasks

Work is not finished until its Torque task says so. "RESULT: PR opened" in a
message does not move the task.

- Whoever merges (worker, reviewer, coordinator or manager) transitions the task
  with `torque_task_transition`: `done` once merged, `review` if the work is
  finished but not merged, `blocked` with a reason if it cannot proceed.
- Cite the PR or commit and what was verified in the transition comment or in
  `torque_comment_add`. Record deploy state too (deployed, not deployed, or not
  applicable); do not deploy unless your task says to.
- A reviewer verifies the cited PR or commit and the stated checks before the task
  goes to `done`.
- File any gap you found but did not fix as a new Torque task rather than leaving
  it in prose.
- Do this before you stop. A session that ends first leaves the task stale.
