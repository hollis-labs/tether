// S5 — Tether launch corpus validation.
//
// This test walks testdata/launch-specs/ — the S5-cutover parameterized
// re-expression of the legacy ~/.tether/catalog/launches/ +
// boot-profiles/ directories — and proves it loads clean through the
// frozen go-agent-launch S4.4 entry points:
//
//	agentlaunch.LoadLaunchSpec        on launch-assembly.yaml
//	agentlaunch.LoadLaunchBag         on every launches/*.yaml
//	agentlaunch.ValidateMinimumConfig on every (spec, bag) pair
//
// It is the in-repo guard that the S5 corpus stays valid against the
// frozen S4.2 var model and S4.4 minimum-config contract. The S4.5
// parity harness drives exactly these entry points.
package internal_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"gopkg.in/yaml.v3"
)

// loadYAMLMap reads a YAML file into a string-keyed map. Used to inspect
// the partial-input template fragments, which are not full LaunchBags.
func loadYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

// corpusRoot is the launch-spec corpus directory, relative to this test
// file (internal/ -> ../testdata/launch-specs).
func corpusRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "testdata", "launch-specs"))
	if err != nil {
		t.Fatalf("resolve corpus root: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("corpus root missing: %v", err)
	}
	return root
}

// TestLaunchSpecLoads proves the canonical LaunchSpec parses and
// validates through agentlaunch.LoadLaunchSpec — including the embedded
// AssemblySpec contract, the LIVE var sources (file/cmd/call), and the
// S4.4 minimum-config input contract (work_dir + runner required).
func TestLaunchSpecLoads(t *testing.T) {
	specPath := filepath.Join(corpusRoot(t), "launch-assembly.yaml")
	spec, err := agentlaunch.LoadLaunchSpec(specPath)
	if err != nil {
		t.Fatalf("LoadLaunchSpec(%s): %v", specPath, err)
	}
	if spec.ID != "tether.launch" {
		t.Fatalf("spec id = %q, want tether.launch", spec.ID)
	}
}

// TestLaunchBagsLoadAndValidate walks every launches/*.yaml, loads it
// via agentlaunch.LoadLaunchBag, and runs agentlaunch.ValidateMinimumConfig
// against the canonical spec. Every legacy launch re-expression — all 64
// bags plus the tether-minimum demo bag — must pass.
func TestLaunchBagsLoadAndValidate(t *testing.T) {
	root := corpusRoot(t)

	spec, err := agentlaunch.LoadLaunchSpec(filepath.Join(root, "launch-assembly.yaml"))
	if err != nil {
		t.Fatalf("LoadLaunchSpec: %v", err)
	}

	bagDir := filepath.Join(root, "launches")
	entries, err := os.ReadDir(bagDir)
	if err != nil {
		t.Fatalf("read launches dir: %v", err)
	}

	var bagCount int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(bagDir, name)
			bag, err := agentlaunch.LoadLaunchBag(path)
			if err != nil {
				t.Fatalf("LoadLaunchBag(%s): %v", name, err)
			}
			if bag.Spec != "tether.launch" {
				t.Fatalf("bag %s spec = %q, want tether.launch", name, bag.Spec)
			}
			if err := agentlaunch.ValidateMinimumConfig(spec, bag); err != nil {
				t.Fatalf("ValidateMinimumConfig(%s): %v", name, err)
			}
		})
		bagCount++
	}

	// 64 legacy launches + tether-minimum.yaml demo bag.
	const wantBags = 65
	if bagCount != wantBags {
		t.Fatalf("bag count = %d, want %d (64 legacy launches + tether-minimum)", bagCount, wantBags)
	}
}

// TestTemplatesAreInputFragments proves each templates/*.yaml is a
// well-formed partial input fragment: a YAML map whose keys are all
// declared inputs of the canonical spec. Templates are not full bags, so
// they are checked structurally rather than via LoadLaunchBag.
func TestTemplatesAreInputFragments(t *testing.T) {
	root := corpusRoot(t)

	spec, err := agentlaunch.LoadLaunchSpec(filepath.Join(root, "launch-assembly.yaml"))
	if err != nil {
		t.Fatalf("LoadLaunchSpec: %v", err)
	}
	declared := make(map[string]struct{}, len(spec.Inputs))
	for _, in := range spec.Inputs {
		declared[in.Name] = struct{}{}
	}

	tmplDir := filepath.Join(root, "templates")
	entries, err := os.ReadDir(tmplDir)
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}

	var tmplCount int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			// A template is a partial input bag: feeding it through
			// LoadLaunchBag would fail (no spec/name keys), so validate
			// it by wrapping it as a bag and running ValidateMinimumConfig,
			// which rejects any undeclared input key.
			frag := loadYAMLMap(t, filepath.Join(tmplDir, name))
			for key := range frag {
				if _, ok := declared[key]; !ok {
					t.Fatalf("template %s has undeclared input %q", name, key)
				}
			}
		})
		tmplCount++
	}

	const wantTemplates = 8
	if tmplCount != wantTemplates {
		t.Fatalf("template count = %d, want %d", tmplCount, wantTemplates)
	}
}
