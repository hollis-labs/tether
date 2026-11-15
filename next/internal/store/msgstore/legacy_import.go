package msgstore

import (
	"context"
	"database/sql"
	"fmt"
)

// The old ImportLegacyMessagesIntoDelivery (delivery_legacy_import.go) was an operator-invoked,
// one-time backfill of delivery-core obligations for messages that predate it, wired to nothing at
// start-up. Its policy and its library calls (Enqueue, then a held or replayed timeline per row) are
// migration work and go with migrate-from (S12); the store keeps the two Tether-table primitives the
// importer needs.

// LegacyImportRow is a mailbox message with no delivery-core tracking.
type LegacyImportRow struct {
	ID, Kind, FromURN, ToURN                                     string
	Channel, ThreadID, InReplyTo, Payload, ContentType, Metadata string
	CreatedAt                                                    string
	DeliveredAt, ConsumedAt, CanceledAt, GroupURN                sql.NullString
}

// LegacyImportCandidates returns every mailbox message without a delivery_id (not staged, not a
// channel publication, not a routing reply), oldest first: the old importer's selection, with its
// skip rules (canceled, group) left to the importer.
func (s *Store) LegacyImportCandidates(ctx context.Context) ([]LegacyImportRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, `SELECT id, kind, COALESCE(channel,''), from_urn, to_urn, COALESCE(thread_id,''), COALESCE(in_reply_to,''),
 COALESCE(payload,''), COALESCE(content_type,''), COALESCE(metadata,''), created_at, delivered_at, consumed_at, canceled_at, group_urn
 FROM messages
 WHERE delivery_id IS NULL AND routing_staged = 0
 AND NOT EXISTS (SELECT 1 FROM channel_publications p WHERE p.message_id = messages.id)
 AND NOT EXISTS (SELECT 1 FROM routing_replies r WHERE r.reply_id = messages.id)
 ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("legacy import: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LegacyImportRow
	for rows.Next() {
		var l LegacyImportRow
		if err := rows.Scan(&l.ID, &l.Kind, &l.Channel, &l.FromURN, &l.ToURN, &l.ThreadID, &l.InReplyTo, &l.Payload, &l.ContentType,
			&l.Metadata, &l.CreatedAt, &l.DeliveredAt, &l.ConsumedAt, &l.CanceledAt, &l.GroupURN); err != nil {
			return nil, fmt.Errorf("legacy import: scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetLegacyDeliveryID records the delivery-core id of an imported message. The write is guarded on
// delivery_id IS NULL, so a second importer racing on the same row records nothing: ok is false and
// the caller knows its new obligation is a duplicate to hold. messages.id and every other column stay
// as they are.
func (s *Store) SetLegacyDeliveryID(ctx context.Context, messageID, deliveryID string) (ok bool, err error) {
	n, err := s.execC(ctx, `UPDATE messages SET delivery_id = ? WHERE id = ? AND delivery_id IS NULL`, deliveryID, messageID)
	if err != nil {
		return false, fmt.Errorf("legacy import: row %s: record delivery_id: %w", messageID, err)
	}
	return n == 1, nil
}
