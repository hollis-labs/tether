package eventretention

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// Record is an adapter input, not an event envelope. Policy is resolved from
// the current configuration for the row's trusted class on each pass. RolledUp
// means every configured bucket was committed atomically with the rollup
// checkpoint. Pending marks an unpublished outbox row.
type Record struct {
	Cursor         int64
	RecordedAt     time.Time
	Policy         Policy
	PII            bool
	Secret         bool
	Pending        bool
	PayloadPresent bool
	RolledUp       bool
}

type Decision struct {
	Tombstone    bool
	TrimEligible bool
	NeedsRollup  bool
}

func validate(r Record) error {
	if r.Cursor <= 0 || r.RecordedAt.IsZero() {
		return fmt.Errorf("retention: positive cursor and recorded time required")
	}
	if r.Secret {
		return fmt.Errorf("retention: secret events must never be stored")
	}
	return nil
}

// Plan determines a row's eligibility without changing the row. Age is measured
// from the log's recorded time, not producer time. PII caps only payload age;
// it does not shorten retention of an audit envelope or its rollups.
func Plan(r Record, now time.Time) (Decision, error) {
	if err := validate(r); err != nil {
		return Decision{}, err
	}
	if r.Pending || r.Policy.epoch == "" || now.Before(r.RecordedAt) {
		return Decision{}, nil
	}
	age := now.Sub(r.RecordedAt)
	rule := r.Policy.rule
	full := rule.Full
	if r.PII && r.Policy.piiCap > 0 && (full == 0 || r.Policy.piiCap < full) {
		full = r.Policy.piiCap
	}
	payloadExpired := full > 0 && age >= time.Duration(full)
	horizon := rule.Full
	if rule.Full != 0 && rule.Envelope >= 0 {
		horizon = rule.Envelope
	}
	return Decision{
		Tombstone:    r.PayloadPresent && payloadExpired,
		TrimEligible: r.Policy.class != Audit && horizon > 0 && age >= time.Duration(horizon) && r.RolledUp,
		NeedsRollup:  !r.RolledUp,
	}, nil
}

// HeadTrim returns the last cursor of a deletable contiguous prefix and its
// length. rows MUST start at the current log head, in ascending cursor order;
// gaps in numeric IDs are allowed. Never select only old rows before calling:
// a retained, pending, legacy or unrolled row is a barrier, not a row to skip.
func HeadTrim(rows []Record, now time.Time, limit int) (int64, int, error) {
	if limit <= 0 {
		return 0, 0, fmt.Errorf("retention: positive batch limit required")
	}
	if err := validateOrder(rows); err != nil {
		return 0, 0, err
	}
	var through int64
	count := 0
	for _, r := range rows {
		d, err := Plan(r, now)
		if err != nil {
			return 0, 0, err
		}
		if !d.TrimEligible || count == limit {
			break
		}
		through = r.Cursor
		count++
	}
	return through, count, nil
}

// Tombstone returns the replacement payload (always absent) and digest. The
// caller must use it only when Plan permits removal, set only the payload and
// digest columns, and preserve envelope metadata including payload_schema,
// content_type and truncated. nil means already absent and yields no digest.
// PII payloads use the sentinel "withheld" to resist dictionary attacks while
// remaining distinguishable from events that never had a payload.
func Tombstone(payload []byte, pii bool) ([]byte, string) {
	if payload == nil {
		return nil, ""
	}
	if pii {
		return nil, "withheld"
	}
	sum := sha256.Sum256(payload)
	return nil, "sha256:" + hex.EncodeToString(sum[:])
}

// Rollup contains only low-cardinality class/sensitivity dimensions plus a
// configuration epoch. No subject, correlation, actor, payload or kind is used
// as a dimension. Epoch prevents new keep durations shortening older buckets.
type Rollup struct {
	Epoch       string
	Class       Class
	PII         bool
	Bucket      time.Duration
	Start       time.Time
	Keep        time.Duration // zero = forever; expiry measured from bucket end
	Count       int64
	FirstCursor int64
	LastCursor  int64
}

type rollupKey struct {
	epoch  string
	class  Class
	pii    bool
	bucket time.Duration
	start  time.Time
}

// RollupBatch returns count deltas and a checkpoint. The adapter MUST apply
// both in one transaction; the next call supplies that committed checkpoint as
// after. Replaying committed cursors yields no deltas. Callers provide the full
// ordered log stream (not a filtered class); pending/legacy rows stop progress.
// Multiple bucket sizes are counted directly from rows, never from other counts.
func RollupBatch(rows []Record, after int64) ([]Rollup, int64, error) {
	if after < 0 {
		return nil, after, fmt.Errorf("retention: nonnegative checkpoint required")
	}
	if err := validateOrder(rows); err != nil {
		return nil, after, err
	}
	sums := map[rollupKey]Rollup{}
	cursor := after
	for _, r := range rows {
		if r.Cursor <= after {
			continue
		}
		if r.Pending || r.Policy.epoch == "" {
			break
		}
		for _, rule := range r.Policy.rule.Rollups {
			bucket := time.Duration(rule.Bucket)
			key := rollupKey{r.Policy.epoch, r.Policy.class, r.PII, bucket, r.RecordedAt.UTC().Truncate(bucket)}
			sum := sums[key]
			if sum.Count == 0 {
				sum = Rollup{Epoch: key.epoch, Class: key.class, PII: key.pii, Bucket: bucket, Start: key.start, Keep: time.Duration(rule.Keep), FirstCursor: r.Cursor}
			}
			sum.Count++
			sum.LastCursor = r.Cursor
			sums[key] = sum
		}
		cursor = r.Cursor
	}
	out := make([]Rollup, 0, len(sums))
	for _, sum := range sums {
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Epoch != b.Epoch {
			return a.Epoch < b.Epoch
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		if a.PII != b.PII {
			return !a.PII
		}
		if a.Bucket != b.Bucket {
			return a.Bucket < b.Bucket
		}
		return a.Start.Before(b.Start)
	})
	return out, cursor, nil
}

func validateOrder(rows []Record) error {
	var previous int64
	for _, r := range rows {
		if err := validate(r); err != nil {
			return err
		}
		if r.Cursor <= previous {
			return fmt.Errorf("retention: records must be in ascending cursor order")
		}
		previous = r.Cursor
	}
	return nil
}

func (r Rollup) Expired(now time.Time) bool {
	return r.Keep > 0 && !now.Before(r.Start.Add(r.Bucket).Add(r.Keep))
}
