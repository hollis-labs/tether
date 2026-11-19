- Seat status needs a verified setter (CW-20261114-0130): in observe mode an unverified `?as=` claim on `POST /status`
  was recorded but not refused, so naming a manager seat let anyone set any seat's status. A claim the hub cannot
  verify now sets nothing (403) in every mode; the seat itself, a verified catalog manager and the operator still do
  (identity off on the local socket unchanged). The status records `set_by_verification` and `set_by_principal`
  (migration 0104), on the card and in `seat.status`.
