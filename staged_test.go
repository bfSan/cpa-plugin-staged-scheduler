package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
// A strict ladder is a whitelist: when no account it names is available, the pick
// is refused rather than handed back. Handing back was the hole this closes --
// the built-in selector would then pick a credential the operator never listed,
// so the ladder was a preference and not a boundary.
func TestStagedStrictRejectsWhenNothingInTheLadderIsAvailable(t *testing.T) {
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
	if !got.Handled || !got.Reject {
		t.Fatalf("pick = %+v, want a rejection when strict and no listed account is available", got)
	}
	if got.AuthID != "" {
		t.Errorf("AuthID = %q, want empty: a rejection selects nothing", got.AuthID)
	}
	if got.RejectCode != "auth_unavailable" {
		t.Errorf("RejectCode = %q, want auth_unavailable", got.RejectCode)
	}
	if !strings.Contains(got.RejectReason, "gpt-6.1-sol") {
		t.Errorf("RejectReason = %q, want it to name the model", got.RejectReason)
	}
}

// The same ladder under exclude keeps the old escape hatch, so an operator who
// wants a fallback can still have one. Only the default changed.
func TestStagedExcludeHandsBackWhenNothingInTheLadderIsAvailable(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  gpt-6.1-sol:
    strategy: staged
    unlisted: exclude
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
	if got.Handled || got.Reject {
		t.Fatalf("pick = %+v, want a hand-back when exclude and no listed account is available", got)
	}
}

// An empty candidate set is the same decision point as a spent ladder: none of
// the named accounts can serve the model. A strict ladder must refuse here too,
// otherwise the host could bypass the whitelist simply by offering fewer
// candidates.
func TestStagedStrictRejectsOnEmptyCandidates(t *testing.T) {
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

	got := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "gpt-6.1-sol"})
	if !got.Handled || !got.Reject {
		t.Fatalf("pick = %+v, want a rejection on an empty candidate set", got)
	}
}

// The fallback pool is a stage, not a mode. Naming the remaining accounts in the
// final stage is how "burn A, then spread across everything else" is expressed,
// and it is the better spelling: the pool is visible in the ladder and can carry
// weights, neither of which a flag that appended unlisted candidates could do.
func TestStagedFallbackPoolIsAFinalStage(t *testing.T) {
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
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	// A is spent, so the ladder falls through to the pool and rotates there.
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		got := plugin.Pick(pluginapi.SchedulerPickRequest{
			Model: "gpt-6.1-sol",
			Candidates: []pluginapi.SchedulerAuthCandidate{
				codexCandidate("C"), codexCandidate("D"),
			},
		})
		if !got.Handled || got.AuthID == "" {
			t.Fatalf("pick %d = %+v, want the fallback pool to serve", i, got)
		}
		seen[got.AuthID]++
	}
	if seen["C"] != 2 || seen["D"] != 2 {
		t.Errorf("pool split = %v, want C and D to rotate evenly", seen)
	}
}

// A credential in no stage is never picked, even when the ladder is otherwise
// spent, because the account list is the whole pool.
func TestStagedAccountOutsideEveryStageIsNeverPicked(t *testing.T) {
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
		Model: "gpt-6.1-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			codexCandidate("C"), codexCandidate("D"),
		},
	})
	if got.AuthID != "" {
		t.Fatalf("pick = %+v, want no credential outside the stages", got)
	}
	if !got.Reject {
		t.Fatalf("pick = %+v, want a refusal since strict is the default", got)
	}
}

// Strict is the default: naming accounts is a statement about the whole pool, so
// an unmentioned credential must not receive traffic -- and when the ladder has
// nothing left, that has to be refused rather than handed back, because a
// hand-back lets the built-in selector pick exactly the credential that was
// never named.
func TestStagedStrictIsTheDefaultAndRefusesUnlisted(t *testing.T) {
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

	// A healthy ladder is unaffected: it never looks at the unlisted credential.
	got := plugin.Pick(pluginapi.SchedulerPickRequest{
		Model:      "gpt-6.1-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("A"), codexCandidate("C")},
	})
	if !got.Handled || got.AuthID != "A" {
		t.Fatalf("pick = %+v, want A while the ladder has an account", got)
	}

	// Once only the unlisted credential remains, strict refuses instead of
	// letting C through.
	got = plugin.Pick(pluginapi.SchedulerPickRequest{
		Model:      "gpt-6.1-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{codexCandidate("C")},
	})
	if !got.Reject {
		t.Fatalf("pick = %+v, want a rejection so the unlisted credential is never used", got)
	}
}

// The default has to be visible in the snapshot, because the panel shows it and
// the config may have been written before the mode existed.
func TestStagedEmptyUnlistedNormalisesToStrict(t *testing.T) {
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
	snaps := plugin.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("snapshots() = %d rows, want 1", len(snaps))
	}
	if snaps[0].Unlisted != unlistedStrict {
		t.Errorf("Unlisted = %q, want %q", snaps[0].Unlisted, unlistedStrict)
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
			// The fallback pool belongs in the ladder's final stage. A mode that
			// appended every unlisted candidate would make the account list stop
			// describing the whole pool, so it is not accepted at all -- including
			// the name it used to have, which must not read as a valid choice.
			name: "removed include mode",
			config: `rules:
  model-a:
    strategy: staged
    unlisted: include
    stages:
      - name: primary
        mode: first
        accounts: [A]
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

// 复现：对「还没有保存规则」的模型做试算时，probe 会合成一条零值规则，
// 其 Unlisted 是 ""，而 "" 不等于 "strict"。于是试算显示「交回 CPA」，
// 但真存下来之后（normalize 补成 strict）实际是「拒绝」—— 界面在保存前
// 给出与实际相反的行为说明。
func TestProbeOnUnconfiguredModelMatchesSavedBehaviour(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`rules: {}`)); err != nil {
		t.Fatal(err)
	}
	// 模型没有规则；面板选择 staged 策略后试算，候选里没有阶梯账号。
	steps := plugin.probe("brand-new-model", "staged", []pluginapi.SchedulerAuthCandidate{codexCandidate("Z")}, 1)
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	t.Logf("未配置模型 + staged 试算 → handled=%v reject=%v", steps[0].Handled, steps[0].Reject)

	// 保存同样的规则后再试算，两者必须一致。
	if err := plugin.Reconfigure([]byte(`
rules:
  brand-new-model:
    strategy: staged
    stages:
      - name: s1
        mode: first
        accounts: [A]
`)); err != nil {
		t.Fatal(err)
	}
	saved := plugin.probe("brand-new-model", "staged", []pluginapi.SchedulerAuthCandidate{codexCandidate("Z")}, 1)
	t.Logf("已保存规则   + staged 试算 → handled=%v reject=%v", saved[0].Handled, saved[0].Reject)

	if steps[0].Reject != saved[0].Reject {
		t.Errorf("试算与保存后行为不一致：未保存 reject=%v，已保存 reject=%v", steps[0].Reject, saved[0].Reject)
	}
}
