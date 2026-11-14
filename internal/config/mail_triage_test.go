package config

import (
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestEffectiveMailTriage(t *testing.T) {
	on, off := true, false
	var g Global
	if en, iv := EffectiveMailTriage(g, Agent{}); en || iv != DefaultMailTriageActionInterval {
		t.Fatalf("default: %v %v", en, iv)
	}
	if en, iv := EffectiveMailTriage(g, Agent{MailTriage: &launchprofile.MailTriage{Enabled: &on, ActionInterval: "90s"}}); !en || iv != 90*time.Second {
		t.Fatalf("agent: %v %v", en, iv)
	}
	g.Catalog.Defaults.MailTriage = MailTriageDefaults{Enabled: true, ActionInterval: "5m"}
	if en, iv := EffectiveMailTriage(g, Agent{}); !en || iv != 5*time.Minute {
		t.Fatalf("fleet: %v %v", en, iv)
	}
	if en, _ := EffectiveMailTriage(g, Agent{MailTriage: &launchprofile.MailTriage{Enabled: &off}}); en {
		t.Fatal("agent opt-out ignored")
	}
	bad := &Catalog{Agents: map[string]Agent{"a": {ID: "a", MailTriage: &launchprofile.MailTriage{ActionInterval: "soon"}}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid interval accepted")
	}
}
