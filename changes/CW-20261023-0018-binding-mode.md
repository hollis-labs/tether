- Add `lifecycle.binding: replace | keep | none` to launch profiles and launch files
  (CW-20261023-0018): what a launch does with its logical agent's mail address. `replace` (the
  default, unset) is unchanged: the newest launch takes it. `keep` takes it only when no other session
  still holds it (a resume or replacement of the holder still takes it); `none` never takes it, so the
  session is addressed by session URN. For a shared agent running several sessions at once. When `keep`
  declines, the launch response carries `binding_held_by`. An unknown value is a catalog error.
