- A seat's own live session can set its own status again (CW-20261114-0130, the #568 regression). A session credential
  whose own (non-terminal) session is bound to the seat's logical agent counts as the seat: without `?as=` the status
  is the seat's, not the session URN's; with `?as=` naming the seat (any of its forms) it verifies; `target: "@self"`
  means the caller. Any other seat's session is still refused.
