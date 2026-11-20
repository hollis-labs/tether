-- A recipient handoff and its event are committed atomically. First handoff wins.
ALTER TABLE messages ADD COLUMN delivery_path TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN delivery_session_id TEXT NOT NULL DEFAULT '';
DROP TRIGGER message_events_delivered;
CREATE TRIGGER message_events_delivered AFTER UPDATE OF delivered_at ON messages
 WHEN OLD.delivered_at IS NULL AND NEW.delivered_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.delivered',NEW.delivered_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'path',NEW.delivery_path,'session_id',NEW.delivery_session_id,
 'delivery',json_object('state','delivered','path',NEW.delivery_path,'session_id',NEW.delivery_session_id,'delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;

-- Any explicit read also proves delivery, including legacy rows with no delivery-core row.
CREATE TRIGGER message_delivery_read_implied AFTER UPDATE OF read_at ON messages
WHEN NEW.read_at IS NOT NULL AND NEW.delivered_at IS NULL
BEGIN
 UPDATE messages SET delivered_at=NEW.read_at, delivery_path='read_implied'
 WHERE id=NEW.id AND delivered_at IS NULL;
END;
UPDATE messages SET delivered_at=read_at, delivery_path='read_implied'
WHERE read_at IS NOT NULL AND delivered_at IS NULL;
