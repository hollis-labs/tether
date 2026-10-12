package workerwire

const PushViaOutpost = "push-via-outpost"

// Binding contains routing data only. Adapter names/handles and capabilities
// stay on the machine; the hub cannot choose a command to execute there.
type Binding struct {
	ID             string   `json:"id"`
	URN            string   `json:"urn,omitempty"`
	Handle         string   `json:"handle"`
	TTL            int      `json:"ttl"`
	Generation     int64    `json:"generation,omitempty"`
	AllowedSenders []string `json:"allowed_senders,omitempty"`
}
