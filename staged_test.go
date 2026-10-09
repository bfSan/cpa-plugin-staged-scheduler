package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The staged strategy exists to express one thing: burn account A, then B, then
// spread across a pool. These tests pin that meaning, because it is the whole
// reason the plugin is a fork rather than the upstream original.

func codexCandidate(name string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: name, Provider: "codex", Status: "active"}
}

// A ladder of single-account stages must stay on the same account for as long as
// the host keeps offering it. That is what "burn A before touching B" means: the
// plugin does not rotate on its own.
func TestStagedFirstStageKeepsBurningTheSameAccount(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A, B, C]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{codexCandidate("A"), codexCandidate("B"), codexCandidate("C")}
	for i := 0; i < 5; i++ {
		got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "gpt-6.1-sol", Candidates: candidates})
		if !got.Handled || got.AuthID != "A" {
			t.Fatalf("pick %d = %+v, want A every time", i, got)
		}
	}
}

// The advance to the next stage is driven by the host withdrawing the first
// account from Candidates (cooldown, disable, quota error). The plugin must not
// need a quota API of its own to notice.
func TestStagedAdvancesWhenTheHostWithdrawsTheAccount(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
      - name: secondary
        mode: first
        accounts: [B]
      - name: pool
        mode: round-robin
        accounts: [C, D, E]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	withA := []pluginapi.SchedulerAuthCandidate{codexCandidate("A"), codexCandidate("B"), codexCandidate("C")}
	if got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "gpt-6.1-sol", Candidates: withA}); got.AuthID != "A" {
		t.Fatalf("with A available: pick = %+v, want A", got)
	}

	// A is now spent and the host stops offering it.
	withoutA := []pluginapi.SchedulerAuthCandidate{codexCandidate("B"), codexCandidate("C")}
	if got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "gpt-6.1-sol", Candidates: withoutA}); got.AuthID != "B" {
		t.Fatalf("after A is withdrawn: pick = %+v, want B", got)
	}

	// B is spent too: the chain falls into the pool.
	poolOnly := []pluginapi.SchedulerAuthCandidate{codexCandidate("C"), codexCandidate("D"), codexCandidate("E")}
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "gpt-6.1-sol", Candidates: poolOnly})
		if !got.Handled {
			t.Fatalf("pool pick = %+v, want handled", got)
		}
		seen[got.AuthID] = true
	}
	if !seen["C"] || !seen["D"] || !seen["E"] {
		t.Fatalf("pool rotation = %v, want all of C, D, E", seen)
	}
}

// Once the whole ladder is exhausted the plugin must hand the request back rather
// than invent a selection.
func TestStagedHandsBackWhenNothingInTheLadderIsAvailable(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	got := plugin.Pick(pluginapi.SchedulerPickRequest{
		Model:      "gpt-6.1-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("Z")},
	})
	if got.Handled || got.AuthID != "" {
		t.Fatalf("pick = %+v, want unhandled when no listed account is available", got)
	}
}

// "Unlisted: include" is how the operator says "after my named accounts, spill
// into everything else" without having to enumerate the remainder.
func TestStagedUnlistedIncludeSpillsIntoTheFinalStage(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    unlisted: include
    stages:
      - name: primary
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	// A is gone; only unlisted credentials remain.
	got := plugin.Pick(pluginapi.SchedulerPickRequest{
		Model:      "gpt-6.1-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("C"), codexCandidate("D")},
	})
	if !got.Handled {
		t.Fatalf("pick = %+v, want the unlisted credential to be picked", got)
	}
	if got.AuthID != "C" {
		t.Fatalf("pick = %+v, want the first unlisted credential in sorted order", got)
	}
}

// Exclude is the default: naming accounts is a statement about the whole pool, so
// an unmentioned credential must not silently receive traffic.
func TestStagedExcludesUnlistedAccountsByDefault(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	got := plugin.Pick(pluginapi.SchedulerPickRequest{
		Model:      "gpt-6.1-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("C")},
	})
	if got.Handled {
		t.Fatalf("pick = %+v, want unlisted credential to be ignored", got)
	}
}

// Weights inside a stage decide the split; 2:1 must be observable over a run.
func TestStagedWeightedStageHonoursWeights(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: pool
        mode: weighted-round-robin
        accounts: [C, D]
        weights:
          C: 2
          D: 1
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{codexCandidate("C"), codexCandidate("D")}
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "gpt-6.1-sol", Candidates: candidates})
		if !got.Handled {
			t.Fatalf("pick %d = %+v, want handled", i, got)
		}
		counts[got.AuthID]++
	}
	if counts["C"] != 6 || counts["D"] != 3 {
		t.Fatalf("weighted split = %v, want C 6 and D 3", counts)
	}
}

// Two models that share an account pool must rotate independently, otherwise
// adding a second model would silently change the first model's behaviour.
func TestStagedCursorsAreIndependentPerModel(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  model-a:
    strategy: staged
    stages:
      - name: pool
        mode: round-robin
        accounts: [A, B]
  model-b:
    strategy: staged
    stages:
      - name: pool
        mode: round-robin
        accounts: [A, B]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{codexCandidate("A"), codexCandidate("B")}
	firstA := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "model-a", Candidates: candidates})
	firstB := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "model-b", Candidates: candidates})
	if firstA.AuthID != "A" || firstB.AuthID != "A" {
		t.Fatalf("first picks = %q and %q, want both models to start at A", firstA.AuthID, firstB.AuthID)
	}

	secondA := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "model-a", Candidates: candidates})
	if secondA.AuthID != "B" {
		t.Fatalf("model-a second pick = %q, want B", secondA.AuthID)
	}
}

// A ladder whose accounts all sit in one tier must still work when the host, for
// whatever reason, hands over every tier at once; the stage order, not the host's
// priority field, decides who is used first.
func TestStagedOrderFollowsTheLadderNotTheHostPriority(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
      - name: secondary
        mode: first
        accounts: [B]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	// B carries a higher host priority; A is still the ladder's head.
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "A", Provider: "codex", Priority: 1},
		{ID: "B", Provider: "codex", Priority: 99},
	}
	got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "gpt-6.1-sol", Candidates: candidates})
	if got.AuthID != "A" {
		t.Fatalf("pick = %+v, want A regardless of host priority", got)
	}
}

func TestStagedRejectsInvalidLadders(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{
			name: "no stages",
			config: `rules:
  model-a:
    strategy: staged
`,
		},
		{
			name: "stage without accounts",
			config: `rules:
  model-a:
    strategy: staged
    stages:
      - name: primary
        mode: first
`,
		},
		{
			name: "unknown mode",
			config: `rules:
  model-a:
    strategy: staged
    stages:
      - name: primary
        mode: random
        accounts: [A]
`,
		},
		{
			name: "duplicate stage name",
			config: `rules:
  model-a:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
      - name: primary
        mode: first
        accounts: [B]
`,
		},
		{
			name: "account in two stages",
			config: `rules:
  model-a:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
      - name: secondary
        mode: first
        accounts: [A]
`,
		},
		{
			name: "weights on a first stage",
			config: `rules:
  model-a:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
        weights:
          A: 2
`,
		},
		{
			name: "weight for an unlisted account",
			config: `rules:
  model-a:
    strategy: staged
    stages:
      - name: pool
        mode: weighted-round-robin
        accounts: [A]
        weights:
          B: 2
`,
		},
		{
			name: "negative weight",
			config: `rules:
  model-a:
    strategy: staged
    stages:
      - name: pool
        mode: weighted-round-robin
        accounts: [A]
        weights:
          A: -1
`,
		},
		{
			name: "unknown unlisted mode",
			config: `rules:
  model-a:
    strategy: staged
    unlisted: sometimes
    stages:
      - name: primary
        mode: first
        accounts: [A]
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := newSchedulerPlugin()
			if err := plugin.Reconfigure([]byte(test.config)); err == nil {
				t.Fatal("Reconfigure() error = nil, want the ladder to be rejected")
			}
		})
	}
}

// The panel offers this strategy, so the build has to accept it. This is the
// contract that keeps the UI from advertising something Pick cannot honour.
func TestStagedIsAdvertisedAndAccepted(t *testing.T) {
	if !isSupportedStrategy(strategyStaged) {
		t.Fatal("strategyStaged is missing from supportedStrategies")
	}
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
}

// The panel renders the ladder from the snapshot, so the stage order and modes
// have to survive into it unchanged.
func TestSnapshotCarriesTheLadder(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    stages:
      - name: primary
        mode: first
        accounts: [A]
      - name: pool
        mode: weighted-round-robin
        accounts: [C, D]
        weights:
          C: 2
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	snapshots := plugin.snapshots()
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots))
	}
	stages := snapshots[0].Stages
	if len(stages) != 2 {
		t.Fatalf("stages = %+v, want 2", stages)
	}
	if stages[0].Name != "primary" || stages[0].Mode != stageModeFirst || len(stages[0].Accounts) != 1 {
		t.Fatalf("first stage = %+v, want primary/first with one account", stages[0])
	}
	if stages[1].Name != "pool" || stages[1].Mode != stageModeWeightedRoundRobin {
		t.Fatalf("second stage = %+v, want pool/weighted-round-robin", stages[1])
	}
	if stages[1].Weights["C"] != 2 {
		t.Fatalf("pool weights = %+v, want C:2", stages[1].Weights)
	}
}
