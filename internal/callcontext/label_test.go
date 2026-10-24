package callcontext

import (
	"strings"
	"testing"
)

func TestValidClientLabel(t *testing.T) {
	for _, ok := range []string{"tether", "tether-sysadmin", "nanite:architect", "agent@host/role_1.v2", strings.Repeat("a", 64)} {
		if !ValidClientLabel(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 65), "has space", "semi;colon", "new\nline", "quote\"", "ünicode", "tab\t", "a,b", "a=b"} {
		if ValidClientLabel(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
