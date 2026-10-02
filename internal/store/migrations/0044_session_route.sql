-- Resolved opt-in routing snapshot. Legacy sessions remain unrouted.
ALTER TABLE sessions ADD COLUMN route_json TEXT CHECK(route_json IS NULL OR json_valid(route_json));
