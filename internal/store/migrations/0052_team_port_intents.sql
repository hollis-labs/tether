-- Write-ahead receipts for explicitly scheduled team runtime adapters.
-- Construction and migration start no runtime work.
CREATE TABLE team_port_intents (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 port_kind TEXT NOT NULL CHECK(port_kind IN ('enrollment','session','delivery')),
 intent_key TEXT NOT NULL,
 nonce TEXT NOT NULL DEFAULT '',
 binding_secret TEXT NOT NULL DEFAULT '',
 acquired_urn TEXT NOT NULL DEFAULT '',
 request BLOB,
 payload BLOB,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','done','cleaned','failed')),
 ended TEXT NOT NULL DEFAULT '' CHECK(ended IN ('','release','retire','stop')),
 binding_ended INTEGER NOT NULL DEFAULT 0 CHECK(binding_ended IN (0,1)),
 UNIQUE(port_kind,intent_key)
);
CREATE INDEX team_port_pending ON team_port_intents(sequence) WHERE port_kind<>'delivery' AND state IN ('pending','done');
