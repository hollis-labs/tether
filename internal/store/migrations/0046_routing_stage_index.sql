-- Pending stages are the durable retry queue. Publication clears the flag;
-- purged stages have no body and must never return to the router.
CREATE INDEX idx_messages_routing_pending ON messages(id)
WHERE routing_staged=1 AND payload IS NOT NULL;
