package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// stagedWeightedRule is the smallest rule that exercises a weighted stage: one
// stage, no weights, so the rotation is a plain cursor over the pool. Session
// affinity is left off here; tests that need it prepend the switch.
const stagedWeightedRule = `
rules:
  sticky-model:
    strategy: staged
    unlisted: strict
    stages:
      - name: pool
        mode: weighted-round-robin
        accounts:
          - auth-a
          - auth-b
          - auth-c
`

func stickyCandidates() []pluginapi.SchedulerAuthCandidate {
	return []pluginapi.SchedulerAuthCandidate{
		{ID: "auth-a", Provider: "p", Status: "active"},
		{ID: "auth-b", Provider: "p", Status: "active"},
		{ID: "auth-c", Provider: "p", Status: "active"},
	}
}

func pickWithSession(t *testing.T, plugin *schedulerPlugin, session string, candidates []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	t.Helper()
	request := pluginapi.SchedulerPickRequest{
		Model:      "sticky-model",
		Candidates: candidates,
	}
	if session != "" {
		request.Options.Headers = http.Header{"X-Session-Id": []string{session}}
	}
	return plugin.Pick(request)
}

// A session that keeps requesting must keep landing on the same account, even
// though the stage would otherwise rotate on every pick. This is the whole point
// of the change: the host's own stickiness does not run once a plugin answers.
func TestStagedPick_SessionSticksToSameAccount(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte("session_affinity: true\n" + stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	first := pickWithSession(t, plugin, "sess-1", stickyCandidates())
	if first.AuthID == "" {
		t.Fatalf("first pick returned no auth id")
	}
	for i := 0; i < 5; i++ {
		next := pickWithSession(t, plugin, "sess-1", stickyCandidates())
		if next.AuthID != first.AuthID {
			t.Fatalf("pick %d = %q, want the session's bound account %q", i+2, next.AuthID, first.AuthID)
		}
	}
}

// Two different sessions must be able to use different accounts: stickiness is
// per session, not a global pin. Without this the stage would collapse to a
// single account for every client.
func TestStagedPick_DifferentSessionsSpread(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	seen := map[string]bool{}
	for _, session := range []string{"sess-a", "sess-b", "sess-c"} {
		seen[pickWithSession(t, plugin, session, stickyCandidates()).AuthID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("three sessions all landed on %v, want them spread across the pool", seen)
	}
}

// A request with no session header must not be pinned: it should rotate exactly
// as it did before this change, and it must not create a binding that a later
// session-less request would reuse.
func TestStagedPick_NoSessionHeaderRotates(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	first := pickWithSession(t, plugin, "", stickyCandidates()).AuthID
	second := pickWithSession(t, plugin, "", stickyCandidates()).AuthID
	if first == "" || second == "" {
		t.Fatalf("expected both picks to select an account, got %q then %q", first, second)
	}
	if first == second {
		t.Fatalf("session-less picks both returned %q, want rotation", first)
	}
}

// The bound account disappearing from the candidate set must re-bind the session
// rather than fail it. This is what stops a session from being wedged on an
// account that cooled down or was disabled.
func TestStagedPick_RebindsWhenBoundAccountVanishes(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte("session_affinity: true\n" + stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	first := pickWithSession(t, plugin, "sess-x", stickyCandidates())
	if first.AuthID == "" {
		t.Fatalf("first pick returned no auth id")
	}

	// The bound account is gone from what the host offers.
	var without []pluginapi.SchedulerAuthCandidate
	for _, candidate := range stickyCandidates() {
		if candidate.ID != first.AuthID {
			without = append(without, candidate)
		}
	}
	rebound := pickWithSession(t, plugin, "sess-x", without)
	if rebound.AuthID == "" || rebound.AuthID == first.AuthID {
		t.Fatalf("re-bind returned %q, want a different live account", rebound.AuthID)
	}
	if !rebound.Handled {
		t.Fatalf("re-bind was not handled")
	}

	// And the session now sticks to the new account.
	for i := 0; i < 3; i++ {
		if next := pickWithSession(t, plugin, "sess-x", without); next.AuthID != rebound.AuthID {
			t.Fatalf("after re-bind, pick returned %q, want %q", next.AuthID, rebound.AuthID)
		}
	}
}

// A "first" stage must not be pinned by the binding table: its whole purpose is
// to burn the head of the list until the host stops offering it, so re-selecting
// an earlier account after the ladder moved on would defeat the rule.
func TestStagedPick_FirstStageIgnoresSessionBinding(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  sticky-model:
    strategy: staged
    unlisted: strict
    stages:
      - name: burn
        mode: first
        accounts:
          - auth-a
          - auth-b
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	// Bind the session while auth-a is available.
	if got := pickWithSession(t, plugin, "sess-f", stickyCandidates()).AuthID; got != "auth-a" {
		t.Fatalf("first pick = %q, want auth-a", got)
	}
	// auth-a is gone; a "first" stage must move to auth-b even though the
	// session is bound to auth-a.
	got := pickWithSession(t, plugin, "sess-f", []pluginapi.SchedulerAuthCandidate{
		{ID: "auth-b", Provider: "p", Status: "active"},
	}).AuthID
	if got != "auth-b" {
		t.Fatalf("after auth-a vanished, pick = %q, want auth-b", got)
	}
}

// The preview path replays the rotation and must stay deterministic: it passes no
// headers, so no binding is ever created or consulted.
func TestProbe_IgnoresSessionBindings(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	first := plugin.probe("sticky-model", "", stickyCandidates(), 3)
	second := plugin.probe("sticky-model", "", stickyCandidates(), 3)
	for i := range first {
		if first[i].AuthID != second[i].AuthID {
			t.Fatalf("probe step %d differed between runs: %q vs %q", i, first[i].AuthID, second[i].AuthID)
		}
	}
	// The same probe run must still rotate rather than pin one account.
	if first[0].AuthID == first[1].AuthID && first[1].AuthID == first[2].AuthID {
		t.Fatalf("probe pinned a single account %q, want rotation", first[0].AuthID)
	}
}

func TestSessionKey_PrefersMostSpecificHeader(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{"none", http.Header{}, ""},
		{"session id", http.Header{"X-Session-Id": []string{"abc"}}, "X-Session-Id=abc"},
		{"claude wins over generic", http.Header{
			"X-Session-Id":             []string{"generic"},
			"X-Claude-Code-Session-Id": []string{"claude"},
		}, "X-Claude-Code-Session-Id=claude"},
		{"non-canonical spelling", http.Header{"x-session-id": []string{"lower"}}, "X-Session-Id=lower"},
		{"blank value ignored", http.Header{"X-Session-Id": []string{"   "}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionKey(tc.headers); got != tc.want {
				t.Fatalf("sessionKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The feature is off unless the operator turns it on: the default configuration
// must rotate exactly as it did before session affinity existed.
func TestStagedPick_SessionAffinityOffByDefault(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	first := pickWithSession(t, plugin, "sess-off", stickyCandidates()).AuthID
	second := pickWithSession(t, plugin, "sess-off", stickyCandidates()).AuthID
	if first == second {
		t.Fatalf("with the switch off, picks must rotate; both were %q", first)
	}
}

// Turning the switch on must be enough on its own -- no other field required.
func TestStagedPick_SwitchOnEnablesStickiness(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
session_affinity: true
` + stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	first := pickWithSession(t, plugin, "sess-on", stickyCandidates()).AuthID
	for i := 0; i < 3; i++ {
		if got := pickWithSession(t, plugin, "sess-on", stickyCandidates()).AuthID; got != first {
			t.Fatalf("switch on: pick returned %q, want bound %q", got, first)
		}
	}
}

// A stage allow-list restricts stickiness to the named stages. Here the only
// weighted stage is named "pool", so listing it must pin; listing a different
// name must not.
func TestStagedPick_StageAllowList(t *testing.T) {
	withList := func(stages string) *schedulerPlugin {
		plugin := newSchedulerPlugin()
		if err := plugin.Reconfigure([]byte(
			"session_affinity: true\nsession_affinity_stages:\n" + stages + stagedWeightedRule)); err != nil {
			t.Fatalf("Reconfigure() error = %v", err)
		}
		return plugin
	}

	// "pool" is the weighted stage, so listing it keeps stickiness on.
	included := withList("  - pool\n")
	first := pickWithSession(t, included, "sess-in", stickyCandidates()).AuthID
	if next := pickWithSession(t, included, "sess-in", stickyCandidates()).AuthID; next != first {
		t.Fatalf("allow-listed stage did not pin: %q then %q", first, next)
	}

	// A name that matches no stage turns stickiness off for every stage.
	excluded := withList("  - some-other-stage\n")
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		seen[pickWithSession(t, excluded, "sess-out", stickyCandidates()).AuthID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("non-matching allow-list still pinned: %v", seen)
	}
}

// A bad TTL must be rejected loudly rather than silently replaced with a default.
func TestReconfigure_RejectsBadSessionTTL(t *testing.T) {
	for _, ttl := range []string{"not-a-duration", "0s", "-5m"} {
		plugin := newSchedulerPlugin()
		if err := plugin.Reconfigure([]byte("session_affinity: true\nsession_affinity_ttl: " + ttl + "\n" + stagedWeightedRule)); err == nil {
			t.Fatalf("Reconfigure() with ttl %q succeeded, want an error", ttl)
		}
	}
}

// A custom TTL is honoured, and a binding older than it is dropped.
func TestStagedPick_CustomTTLExpiresBinding(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
session_affinity: true
session_affinity_ttl: 1s
` + stagedWeightedRule)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	first := pickWithSession(t, plugin, "sess-ttl", stickyCandidates()).AuthID
	if first == "" {
		t.Fatalf("first pick returned no auth id")
	}
	// Age the binding past the TTL instead of sleeping.
	plugin.mu.Lock()
	binding := plugin.sessionBindings["X-Session-Id=sess-ttl"]
	binding.touched = binding.touched.Add(-2 * time.Second)
	plugin.sessionBindings["X-Session-Id=sess-ttl"] = binding
	plugin.mu.Unlock()

	// An expired binding must not be reused. It is dropped, so the rotation
	// resumes; the next pick is free to differ.
	got := pickWithSession(t, plugin, "sess-ttl", stickyCandidates()).AuthID
	if _, still := plugin.sessionBindings["X-Session-Id=sess-ttl"]; !still {
		t.Fatalf("expected the expired binding to be replaced, table is empty")
	}
	if got == "" {
		t.Fatalf("pick after expiry returned no auth id")
	}
}
