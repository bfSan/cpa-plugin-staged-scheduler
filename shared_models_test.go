package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// A rule's `models` list exists so one ladder can serve an alias and its upstream
// name. The scheduler looks rules up by the exact string the client requested, so
// both keys have to resolve to the same behaviour.

func TestSharedModelsResolveToTheSameLadder(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  qoder-auto:
    strategy: staged
    models: [auto, qoder-ultimate]
    stages:
      - name: primary
        mode: first
        accounts: [A]
      - name: pool
        mode: round-robin
        accounts: [B, C]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{
		codexCandidate("A"), codexCandidate("B"), codexCandidate("C"),
	}
	// Every listed model must pick through the same ladder.
	for _, model := range []string{"qoder-auto", "auto", "qoder-ultimate"} {
		got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: model, Candidates: candidates})
		if !got.Handled || got.AuthID != "A" {
			t.Fatalf("model %q pick = %+v, want A", model, got)
		}
	}
}

// An unknown model must still fall through to the host; expanding the list must
// not accidentally create a catch-all.
func TestSharedModelsDoNotCatchUnlistedModels(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  qoder-auto:
    strategy: staged
    models: [auto]
    stages:
      - name: primary
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	got := plugin.Pick(pluginapi.SchedulerPickRequest{
		Model:      "some-other-model",
		Candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("A")},
	})
	if got.Handled {
		t.Fatalf("pick = %+v, want an unlisted model to be left to the host", got)
	}
}

// The panel writes the full set back, so listing the rule's own key is normal and
// must not be rejected as a duplicate.
func TestSharedModelsTolerateTheOwnerListingItself(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  qoder-auto:
    strategy: staged
    models: [qoder-auto, auto]
    stages:
      - name: primary
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	snapshots := plugin.snapshots()
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want one row for the ladder", len(snapshots))
	}
	if snapshots[0].Model != "qoder-auto" {
		t.Fatalf("row model = %q, want qoder-auto", snapshots[0].Model)
	}
	if len(snapshots[0].SharedWith) != 1 || snapshots[0].SharedWith[0] != "auto" {
		t.Fatalf("SharedWith = %v, want [auto]", snapshots[0].SharedWith)
	}
}

func TestSharedModelsRejectAmbiguousConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{
			name: "same model listed twice",
			config: `rules:
  qoder-auto:
    strategy: round-robin
    models: [auto, auto]
`,
		},
		{
			name: "empty entry",
			config: `rules:
  qoder-auto:
    strategy: round-robin
    models: ["  "]
`,
		},
		{
			name: "model is both its own rule and listed elsewhere",
			config: `rules:
  qoder-auto:
    strategy: round-robin
    models: [auto]
  auto:
    strategy: fill-first
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := newSchedulerPlugin()
			if err := plugin.Reconfigure([]byte(test.config)); err == nil {
				t.Fatal("Reconfigure() error = nil, want the configuration to be rejected")
			}
		})
	}
}

// Two aliases over one ladder are two traffic streams, so each keeps its own
// cursor. Merging them would make the combined order depend on call interleaving.
func TestSharedModelsKeepIndependentCursors(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  alias-a:
    strategy: staged
    models: [alias-b]
    stages:
      - name: pool
        mode: round-robin
        accounts: [X, Y]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{codexCandidate("X"), codexCandidate("Y")}
	firstA := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "alias-a", Candidates: candidates})
	firstB := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "alias-b", Candidates: candidates})
	if firstA.AuthID != "X" || firstB.AuthID != "X" {
		t.Fatalf("first picks = %q and %q, want both aliases to start at X", firstA.AuthID, firstB.AuthID)
	}
	secondA := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "alias-a", Candidates: candidates})
	if secondA.AuthID != "Y" {
		t.Fatalf("alias-a second pick = %q, want Y", secondA.AuthID)
	}
}

// Only the ladder the operator wrote is edited; aliases are derived from it, so
// the panel must not report them as separate rows.
func TestSnapshotsReportOneRowPerLadder(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  alias-a:
    strategy: staged
    models: [alias-b, alias-c]
    stages:
      - name: primary
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	snapshots := plugin.snapshots()
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1 (aliases are not rows of their own)", len(snapshots))
	}
	if !snapshots[0].Owned {
		t.Fatal("the written rule must be reported as owned")
	}
	want := []string{"alias-b", "alias-c"}
	if len(snapshots[0].SharedWith) != len(want) {
		t.Fatalf("SharedWith = %v, want %v", snapshots[0].SharedWith, want)
	}
	for i, model := range want {
		if snapshots[0].SharedWith[i] != model {
			t.Fatalf("SharedWith = %v, want %v", snapshots[0].SharedWith, want)
		}
	}
}

// Expansion must not leak into the configuration the panel writes back: the
// stored rule keeps its models list so a round trip is stable.
func TestSharedModelsRoundTripIsStable(t *testing.T) {
	config := []byte(`
rules:
  alias-a:
    strategy: round-robin
    models: [alias-b]
`)
	first := newSchedulerPlugin()
	if err := first.Reconfigure(config); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	second := newSchedulerPlugin()
	if err := second.Reconfigure(config); err != nil {
		t.Fatalf("re-Reconfigure() error = %v", err)
	}

	a := first.snapshots()
	b := second.snapshots()
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("snapshots = %d and %d, want 1 each", len(a), len(b))
	}
	if a[0].Model != b[0].Model || len(a[0].SharedWith) != len(b[0].SharedWith) {
		t.Fatalf("round trip is not stable: %+v vs %+v", a[0], b[0])
	}
}
