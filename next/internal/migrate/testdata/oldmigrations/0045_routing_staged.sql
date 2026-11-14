-- Turn output is stored once before its router attaches it to a channel.
-- Staging has no inbox, delivery-core or subscription obligation.
ALTER TABLE messages ADD COLUMN routing_staged INTEGER NOT NULL DEFAULT 0 CHECK (routing_staged IN (0, 1));
