- Adopt the released stamp/sanitize/classify eventstage chain before local
  persistence, preserving transaction-safe stamping and observer ordering.
  Default to diagnostic shadow mode; explicit startup enforcement sanitizes
  registered metadata, preserves compatibility payloads without new sanitation
  claims, and fails closed for unknown kinds. Add managed/unmanaged attribution
  acceptance and catalog kind aliases (CW-20261016-0139).
