package telemetry

import "encoding/json"

type byteCounter struct{ size int64 }

func (c *byteCounter) Write(p []byte) (int, error) { c.size += int64(len(p)); return len(p), nil }

// JSONSize counts canonical JSON bytes without retaining a second full encoded
// payload. encoding/json still needs its reusable internal encode buffer. The
// encoder's trailing newline is excluded to match JSON wire payload size.
func JSONSize(value any) (int64, error) {
	var counter byteCounter
	if err := json.NewEncoder(&counter).Encode(value); err != nil {
		return 0, err
	}
	return counter.size - 1, nil
}
