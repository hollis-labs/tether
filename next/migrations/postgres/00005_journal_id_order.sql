-- message_event_journal ids commit in order (Postgres only). The message-event drain reads the journal
-- by id > checkpoint. An identity value can commit after a higher one, so the drain could pass a row
-- that commits late and never emit its event. A statement-level BEFORE INSERT trigger takes a
-- transaction-scoped advisory lock before any row of the statement draws its id, so journal inserts
-- commit in id order. It covers every writer: the message_events_fn and message_events_group_read_fn
-- triggers and msgstore's RecordMessageWake. As for messages.seq (LockMessageSeq) and events
-- (eventstore.LockEventSeq). Lock order in one transaction: messages, then the journal, then events.
-- SQLite has one writer; sqlite/00005_journal_id_order.sql is a placeholder that keeps the numbering.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION message_event_journal_order_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('tether.message_event_journal.id', 0));
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER message_event_journal_order BEFORE INSERT ON message_event_journal
FOR EACH STATEMENT EXECUTE FUNCTION message_event_journal_order_fn();

