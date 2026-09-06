package registry

// group_fanout.go — T04 (messaging vNext, CW-20260906-0035). Reconciles
// group message send onto the T03 delivery core: adds a durable
// per-recipient delivery obligation FOR EACH resolved member alongside the
// existing group-post write, without changing that write at all.
//
// One canonical room body, unchanged (acceptance #2): InsertGroupMessage's
// existing raw-SQL insert into `messages` (group_urn/group_seq) remains the
// SOLE write of message content -- this file never duplicates the payload,
// never writes a second copy of the body, and the group's `group_members`
// read cursor (last_read_seq) is never touched by anything in this file.
// Fanout is purely additive: N delivery-core RecipientDelivery rows
// referencing the SAME one Message the group post already created content
// for (T01 §2.5's structural finding -- group posts previously had NO
// delivery-core representation at all; T04 gives them one without
// disturbing the existing content/read-cursor model).
//
// Frozen recipient set (acceptance requirement): the member list is
// resolved via Storage.ListMembers EXACTLY ONCE, at send time, and handed
// to delivery.Store.Enqueue as the Recipients set for one EnqueueRequest.
// go-messaging's own delivery core already freezes that set as of the
// Enqueue call (confirmed by its own conformance suite's "group fanout
// freezes recipient snapshot and per-recipient outcomes" sub-test, which
// T03 already runs against Tether's actual wiring) -- a member added or
// removed after this Enqueue call never changes the deliveries already
// created for this message. A LATER SetScopedBinding rebind (scoped_bindings.go)
// is a completely separate table this Enqueue call never re-reads, so a
// rebind can never retroactively redirect an already-accepted delivery
// either.
//
// Fanout is best-effort, matching T03's own established pattern (Consume's
// delivery-core recording): the group message content write is
// SendToGroup's real, tested contract; fanout-obligation creation is an
// additive enhancement layered on top. A fanout failure is logged with
// enough detail for manual/T09 reconciliation and does not fail the send --
// the room body existing is what every existing caller depends on, and it
// already committed by the time fanout runs.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
)

// GroupFanoutDeliveryStore is the narrow seam group_fanout.go depends on --
// just Enqueue, not the full delivery.Store interface, so a test stub only
// has to implement the one method this file actually calls. *delivery.SQLiteStore
// (and delivery.Store generally) already satisfies this.
type GroupFanoutDeliveryStore interface {
	Enqueue(ctx context.Context, req delivery.EnqueueRequest) (delivery.EnqueueResult, error)
}

// fanOutGroupMessage resolves grpURN's current members (minus the sender,
// who does not need a delivery obligation for their own post) and enqueues
// one delivery-core Message with one frozen RecipientDelivery per member.
// Returns the delivery-core Message.ID for SetGroupMessageDeliveryMapping,
// or ("", nil) when there is nothing to fan out to (a group with no members
// besides the sender) or fanout is disabled (deliveryStore == nil) -- both
// are non-error, non-failure conditions.
func (s *Service) fanOutGroupMessage(ctx context.Context, gm GroupMessage) (deliveryMessageID string, err error) {
	if s.deliveryStore == nil {
		return "", nil
	}
	members, err := s.storage.ListMembers(ctx, gm.GroupURN)
	if err != nil {
		return "", fmt.Errorf("list members: %w", err)
	}
	from, err := messaging.ParseURN(gm.FromURN)
	if err != nil {
		return "", fmt.Errorf("parse from_urn %q: %w", gm.FromURN, err)
	}
	group, err := messaging.ParseURN(gm.GroupURN)
	if err != nil {
		return "", fmt.Errorf("parse group_urn %q: %w", gm.GroupURN, err)
	}

	var recipients []delivery.RecipientTarget
	for _, m := range members {
		if m.MemberURN == gm.FromURN {
			continue
		}
		addr, err := messaging.ParseURN(m.MemberURN)
		if err != nil {
			return "", fmt.Errorf("parse member urn %q: %w", m.MemberURN, err)
		}
		recipients = append(recipients, delivery.RecipientTarget{Address: addr})
	}
	if len(recipients) == 0 {
		return "", nil
	}

	res, err := s.deliveryStore.Enqueue(ctx, delivery.EnqueueRequest{
		From:        from,
		Group:       group,
		Recipients:  recipients,
		Kind:        messaging.Kind(gm.Kind),
		ThreadID:    gm.ThreadID,
		Payload:     gm.Payload,
		ContentType: gm.ContentType,
		Metadata:    map[string]string{"tether_message_id": gm.ID, "tether_group_seq": fmt.Sprintf("%d", gm.GroupSeq)},
	})
	if err != nil {
		return "", fmt.Errorf("enqueue fanout: %w", err)
	}
	return string(res.Message.ID), nil
}

// sendToGroupWithFanout wraps the existing content write (unchanged) with
// best-effort fanout-obligation creation. Called from SendToGroup.
func (s *Service) sendToGroupWithFanout(ctx context.Context, gm GroupMessage) {
	deliveryMessageID, err := s.fanOutGroupMessage(ctx, gm)
	if err != nil {
		log.Printf("registry: group fanout for message %s (group %s) failed -- room body is durable, only the delivery-core fanout obligation is missing, needs manual/T09 reconciliation: %v", gm.ID, gm.GroupURN, err)
		return
	}
	if deliveryMessageID == "" {
		return
	}
	if err := s.storage.SetGroupMessageDeliveryMapping(ctx, gm.ID, deliveryMessageID); err != nil {
		log.Printf("registry: record group fanout mapping for message %s failed: %v", gm.ID, err)
	}
}

// SetGroupMessageDeliveryMapping records the delivery-core Message.ID that
// a group post's fanout obligations live under. Additive: only touches the
// new messages.delivery_message_id column (migration 0021); never touches
// content, thread_id, group_seq or any attention-state column.
func (s *Storage) SetGroupMessageDeliveryMapping(ctx context.Context, messageID, deliveryMessageID string) error {
	if messageID == "" || deliveryMessageID == "" {
		return errors.New("registry: set group message delivery mapping: messageID and deliveryMessageID are required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET delivery_message_id=? WHERE id=?`, deliveryMessageID, messageID)
	if err != nil {
		return fmt.Errorf("registry: set group message delivery mapping: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registry: set group message delivery mapping: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("registry: set group message delivery mapping: %w: message %s", sql.ErrNoRows, messageID)
	}
	return nil
}
