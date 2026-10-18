package workerwire

import (
	"time"

	re "github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

const FeatureActivity = "activity-v1"

// Activity is a complete, bounded observation of one machine's pulse files.
// It is current state, not a lossless record of hook transitions. An empty
// Events array means the scan succeeded and found no live pulse owners.
type Activity struct {
	ID         string     `json:"id"`
	ObservedAt time.Time  `json:"observed_at"`
	Events     []re.Event `json:"events"`
}

// ActivityReport carries hub-authenticated machine identity separately from
// producer claims. Missing Activity means the worker has not reported yet.
type ActivityReport struct {
	WorkerID   string    `json:"worker_id"`
	Machine    string    `json:"machine"`
	Connected  bool      `json:"connected"`
	ReceivedAt time.Time `json:"received_at"`
	Activity   *Activity `json:"activity,omitempty"`
}
