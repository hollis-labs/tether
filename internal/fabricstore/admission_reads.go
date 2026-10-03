package fabricstore

import "github.com/hollis-labs/substrate/mesh"

// Admission and execution reads use the same reserved writer as their CAS writes.
func (tx *Tx) Admission(caller mesh.URN, key string) (Record[Admission], error) {
	return read[Admission](tx.ctx, tx.conn, admissions, tuple(string(caller), key))
}
func (tx *Tx) Session(urn mesh.URN) (Record[mesh.Session], error) {
	return read[mesh.Session](tx.ctx, tx.conn, sessions, string(urn))
}
func (tx *Tx) Instance(id string) (Record[mesh.AgentInstance], error) {
	return read[mesh.AgentInstance](tx.ctx, tx.conn, instances, id)
}
func (tx *Tx) BindingEvent(id string) (Record[BindingEvent], error) {
	return read[BindingEvent](tx.ctx, tx.conn, history, id)
}
