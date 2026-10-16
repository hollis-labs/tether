// Package eventretention contains dormant D7 retention primitives. It has no
// database, scheduler or bus dependency and does not change the active sweep.
package eventretention

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const Day = 24 * time.Hour
const Year = 365 * Day

// Age is a duration from server-recorded admission time. Zero means forever;
// -1 (YAML "none") disables only the optional envelope tier.
type Age time.Duration

func (a *Age) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!int") {
		return fmt.Errorf("retention duration must be 0, none, or a duration")
	}
	if n.Value == "none" {
		*a = -1
		return nil
	}
	if n.Value == "0" {
		*a = 0
		return nil
	}
	text := n.Value
	multiplier := time.Duration(0)
	if strings.HasSuffix(text, "d") {
		multiplier = Day
	} else if strings.HasSuffix(text, "y") {
		multiplier = Year
	}
	var duration time.Duration
	var err error
	if multiplier != 0 {
		var count int64
		count, err = strconv.ParseInt(text[:len(text)-1], 10, 64)
		if err == nil && (count <= 0 || count > int64((1<<63-1)/multiplier)) {
			err = fmt.Errorf("out of range")
		}
		if err == nil {
			duration = time.Duration(count) * multiplier
		}
	} else {
		duration, err = time.ParseDuration(text)
	}
	if err != nil || duration <= 0 {
		return fmt.Errorf("invalid retention duration %q", text)
	}
	*a = Age(duration)
	return nil
}

type Class string

const (
	Audit    Class = "audit"
	State    Class = "state"
	Activity Class = "activity"
	Debug    Class = "debug"
)

type RollupRule struct {
	Bucket Age `yaml:"bucket" json:"bucket"`
	Keep   Age `yaml:"keep" json:"keep"`
}

type Rule struct {
	Full     Age          `json:"full"`
	Envelope Age          `json:"envelope"`
	Rollups  []RollupRule `json:"rollups"`
}

type rawRule struct {
	Full     *Age          `yaml:"full"`
	Envelope *Age          `yaml:"envelope"`
	Rollups  *[]RollupRule `yaml:"rollups"`
}

type rawConfig struct {
	Retention struct {
		Classes         map[Class]rawRule `yaml:"classes"`
		PIICap          *Age              `yaml:"pii_cap"`
		AcknowledgeRisk bool              `yaml:"acknowledge_risk"`
	} `yaml:"retention"`
}

// Config is an immutable compiled configuration. Its ID identifies the policy
// epoch. Keep a snapshot at admission; never reinterpret old rows using the
// latest Config. Future persistence must commit that snapshot with the event.
type Config struct {
	rules  map[Class]Rule
	piiCap Age
	id     string
}

// Policy is a per-event immutable snapshot from a Config epoch. Zero Policy
// means unclassified legacy data and is never eligible for class-based removal.
type Policy struct {
	class  Class
	rule   Rule
	piiCap Age
	epoch  string
}

func (c Config) ID() string    { return c.id }
func (p Policy) Epoch() string { return p.epoch }
func (p Policy) Class() Class  { return p.class }
func (p Policy) Rule() Rule {
	r := p.rule
	r.Rollups = append([]RollupRule(nil), r.Rollups...)
	return r
}
func (c Config) Snapshot(class Class) (Policy, error) {
	rule, ok := c.rules[class]
	if !ok {
		return Policy{}, fmt.Errorf("retention: unknown or forbidden class %q", class)
	}
	rule.Rollups = append([]RollupRule(nil), rule.Rollups...)
	return Policy{class: class, rule: rule, piiCap: c.piiCap, epoch: c.id}, nil
}

// Parse accepts the proposed retention block, not a whole live catalog. Nothing
// calls this from catalog loading or the daemon yet. Unknown fields are errors.
// Only extending the PII cap or reducing audit's raw retention below 400d needs
// acknowledge_risk; warnings are returned even when the risk is acknowledged.
func Parse(data []byte) (Config, []string, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, nil, fmt.Errorf("retention: expected one YAML document")
	}
	c := Config{piiCap: Age(30 * Day), rules: map[Class]Rule{
		Audit:    {Full: 0, Envelope: -1, Rollups: []RollupRule{{Age(Day), 0}}},
		State:    {Full: Age(180 * Day), Envelope: Age(2 * Year), Rollups: []RollupRule{{Age(time.Hour), Age(2 * Year)}, {Age(Day), 0}}},
		Activity: {Full: Age(30 * Day), Envelope: Age(180 * Day), Rollups: []RollupRule{{Age(time.Hour), Age(2 * Year)}, {Age(Day), 0}}},
		Debug:    {Full: Age(7 * Day), Envelope: -1, Rollups: []RollupRule{{Age(time.Hour), Age(90 * Day)}}},
	}}
	for class, override := range raw.Retention.Classes {
		rule, ok := c.rules[class]
		if !ok {
			return Config{}, nil, fmt.Errorf("retention.classes.%s: unknown or forbidden class (secrets cannot be stored)", class)
		}
		if override.Full != nil {
			rule.Full = *override.Full
		}
		if override.Envelope != nil {
			rule.Envelope = *override.Envelope
		}
		if override.Rollups != nil {
			rule.Rollups = *override.Rollups
		}
		if rule.Full < 0 || rule.Envelope < -1 {
			return Config{}, nil, fmt.Errorf("retention.classes.%s: invalid tier duration", class)
		}
		if rule.Envelope > 0 && (rule.Full == 0 || rule.Envelope < rule.Full) {
			return Config{}, nil, fmt.Errorf("retention.classes.%s: envelope horizon precedes full", class)
		}
		// An explicit empty rollup list would make safe head trim impossible.
		if len(rule.Rollups) == 0 {
			return Config{}, nil, fmt.Errorf("retention.classes.%s: at least one rollup required", class)
		}
		seen := map[Age]bool{}
		for _, r := range rule.Rollups {
			if r.Bucket <= 0 || r.Keep < 0 || seen[r.Bucket] {
				return Config{}, nil, fmt.Errorf("retention.classes.%s: invalid or duplicate rollup bucket/keep", class)
			}
			seen[r.Bucket] = true
		}
		sort.Slice(rule.Rollups, func(i, j int) bool { return rule.Rollups[i].Bucket < rule.Rollups[j].Bucket })
		c.rules[class] = rule
	}
	if raw.Retention.PIICap != nil {
		c.piiCap = *raw.Retention.PIICap
	}
	if c.piiCap < 0 {
		return Config{}, nil, fmt.Errorf("retention.pii_cap: invalid duration")
	}
	warnings := []string{}
	if c.piiCap == 0 || c.piiCap > Age(30*Day) {
		warnings = append(warnings, "retention.pii_cap exceeds the 30d payload cap")
	}
	audit := c.rules[Audit]
	horizon := audit.Full
	if audit.Full != 0 && audit.Envelope >= 0 {
		horizon = audit.Envelope
	}
	if horizon != 0 && horizon < Age(400*Day) {
		warnings = append(warnings, "retention.classes.audit cuts raw audit retention below 400d")
	}
	if len(warnings) > 0 && !raw.Retention.AcknowledgeRisk {
		return Config{}, warnings, fmt.Errorf("retention.acknowledge_risk required: %s", strings.Join(warnings, "; "))
	}
	canonical, err := json.Marshal(struct {
		Rules  map[Class]Rule
		PIICap Age
	}{c.rules, c.piiCap})
	if err != nil {
		return Config{}, nil, err
	}
	sum := sha256.Sum256(canonical)
	c.id = hex.EncodeToString(sum[:])
	return c, warnings, nil
}
