-- Durable metadata-only attention journal; mailbox remains the system of record.
-- Existing attention state is not reconstructed. Delivery transitions are read
-- through mesh/core's public cursor, not intercepted or rewritten.
CREATE TABLE message_event_journal (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 kind TEXT NOT NULL,
 at TEXT NOT NULL,
 payload_json TEXT NOT NULL,
 team_run_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE message_event_deferred (transition_id INTEGER PRIMARY KEY);
CREATE TABLE message_event_checkpoints (
 source TEXT PRIMARY KEY,
 position INTEGER NOT NULL DEFAULT 0
);
INSERT INTO message_event_checkpoints(source) VALUES ('delivery'), ('attention');

CREATE TRIGGER message_events_untracked_sent AFTER INSERT ON messages
 WHEN NEW.delivery_id IS NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.sent',NEW.created_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','sent','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;

CREATE TRIGGER message_events_delivered AFTER UPDATE OF delivered_at ON messages
 WHEN OLD.delivered_at IS NULL AND NEW.delivered_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.delivered',NEW.delivered_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','delivered','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;

CREATE TRIGGER message_events_read AFTER UPDATE OF read_at ON messages
 WHEN OLD.read_at IS NULL AND NEW.read_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.read',NEW.read_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','read','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;

CREATE TRIGGER message_events_consumed AFTER UPDATE OF consumed_at ON messages
 WHEN OLD.consumed_at IS NULL AND NEW.consumed_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.consumed',NEW.consumed_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','consumed','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;

CREATE TRIGGER message_events_archived AFTER UPDATE OF archived_at ON messages
 WHEN OLD.archived_at IS NULL AND NEW.archived_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.archived',NEW.archived_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','archived','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;

CREATE TRIGGER message_events_canceled AFTER UPDATE OF canceled_at ON messages
 WHEN OLD.canceled_at IS NULL AND NEW.canceled_at IS NOT NULL AND NEW.routing_staged=0 AND NEW.kind NOT LIKE 'stream.%' AND coalesce(NEW.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.canceled',NEW.canceled_at,json_object('message_id',NEW.id,'from',NEW.from_urn,'to',json_array(NEW.to_urn),
 'thread_id',coalesce(NEW.thread_id,''),'in_reply_to',coalesce(NEW.in_reply_to,''),
 'msg_kind',NEW.kind,'body_ref','/messages/' || NEW.id,
 'body_bytes',length(CAST(coalesce(NEW.payload,'') AS BLOB)), 'content_type',coalesce(NEW.content_type,''),
 'urgency',CASE WHEN json_valid(NEW.metadata) AND json_extract(NEW.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(NEW.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','canceled','delivery_id',coalesce(NEW.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(NEW.metadata) THEN json_extract(NEW.metadata,'$.team_run') END),''));
 END;

CREATE TRIGGER message_events_purged AFTER UPDATE OF payload,metadata ON messages
 WHEN (OLD.payload IS NOT NULL OR OLD.metadata IS NOT NULL) AND NEW.payload IS NULL AND NEW.metadata IS NULL AND OLD.routing_staged=0 AND OLD.kind NOT LIKE 'stream.%' AND coalesce(OLD.group_urn,'')=''
 BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json,team_run_id)
 VALUES ('message.purged',strftime('%Y-%m-%dT%H:%M:%fZ','now'),json_object('message_id',OLD.id,'from',OLD.from_urn,'to',json_array(OLD.to_urn),
 'thread_id',coalesce(OLD.thread_id,''),'in_reply_to',coalesce(OLD.in_reply_to,''),
 'msg_kind',OLD.kind,'body_ref','/messages/' || OLD.id,
 'body_bytes',length(CAST(coalesce(OLD.payload,'') AS BLOB)), 'content_type',coalesce(OLD.content_type,''),
 'urgency',CASE WHEN json_valid(OLD.metadata) AND json_extract(OLD.metadata,'$.urgency') IN ('very-low','low','normal','high') THEN json_extract(OLD.metadata,'$.urgency') ELSE '' END,
 'delivery',json_object('state','purged','delivery_id',coalesce(OLD.delivery_id,''))),
 coalesce((SELECT run_id FROM team_runs WHERE run_id=CASE WHEN json_valid(OLD.metadata) THEN json_extract(OLD.metadata,'$.team_run') END),''));
 END;

-- A group cursor is an explicit per-member read, not a mutation of the room
-- body. Emit only the newly covered canonical posts for that frozen member.
CREATE TRIGGER message_events_group_read AFTER UPDATE OF last_read_seq ON group_members
WHEN NEW.last_read_seq > OLD.last_read_seq
BEGIN
 INSERT INTO message_event_journal(kind,at,payload_json)
 SELECT 'message.read',strftime('%Y-%m-%dT%H:%M:%fZ','now'),
 json_object('message_id',id,'from',from_urn,'to',json_array(NEW.member_urn),
 'thread_id',coalesce(thread_id,''),'msg_kind',kind,'group',group_urn,'group_seq',group_seq,
 'body_ref','/groups/' || group_urn || '/messages?message_id=' || id,
 'body_bytes',length(CAST(coalesce(payload,'') AS BLOB)), 'content_type',coalesce(content_type,''),
 'delivery',json_object('state','read'))
 FROM messages WHERE group_urn=NEW.grp_urn AND group_seq>OLD.last_read_seq AND group_seq<=NEW.last_read_seq AND kind NOT LIKE 'stream.%';
END;
