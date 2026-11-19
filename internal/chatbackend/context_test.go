package chatbackend

import (
	"encoding/json"
	"testing"
)

func TestTurnInputText(t *testing.T) {
	for _, tc := range []struct {
		name, context, want string
		invalid             bool
	}{
		{name: "absent", want: "hello"},
		{name: "compact", context: `{ "view": "board", "ids": [1, 2] }`, want: "<app-context>{\"view\":\"board\",\"ids\":[1,2]}</app-context>\nhello"},
		{name: "null", context: `null`, want: "<app-context>null</app-context>\nhello"},
		{name: "invalid", context: `{`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (Turn{Text: "hello", Context: json.RawMessage(tc.context)}).InputText()
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("InputText = %q, %v; want %q, invalid %v", got, err, tc.want, tc.invalid)
			}
		})
	}
}
