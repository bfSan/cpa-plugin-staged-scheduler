package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// These tests cover the panel-facing surface: the registration the host consumes,
// the status payload the page renders, and the probe that lets an operator try a
// rule before saving it. The scheduler's own picking behaviour is covered by
// scheduler_test.go.

func TestRegistrationDeclaresSchedulerAndManagement(t *testing.T) {
	reg := pluginRegistration()

	if !reg.Capabilities.Scheduler {
		t.Fatal("Capabilities.Scheduler = false, want true")
	}
	if !reg.Capabilities.ManagementAPI {
		t.Fatal("Capabilities.ManagementAPI = false, want true — the panel would never be served")
	}
	if reg.Metadata.Name != pluginMetadataName || reg.Metadata.Version != pluginVersion {
		t.Fatalf("Metadata = %+v, want name %q version %q", reg.Metadata, pluginMetadataName, pluginVersion)
	}
	if reg.Metadata.GitHubRepository != pluginRepoURL {
		t.Fatalf("GitHubRepository = %q, want %q", reg.Metadata.GitHubRepository, pluginRepoURL)
	}
}

// The panel only offers strategies this build implements, so the advertised list
// and the accepted list must not drift. Every advertised strategy has to be
// accepted by Reconfigure for the trivial rule that uses it.
func TestAdvertisedStrategiesAreAcceptedByReconfigure(t *testing.T) {
	if len(supportedStrategies) == 0 {
		t.Fatal("supportedStrategies is empty")
	}
	for _, strategy := range supportedStrategies {
		plugin := newSchedulerPlugin()
		config := "rules:\n  model-a:\n    strategy: " + strategy + "\n"
		if strategy == strategyStaged {
			// `staged` is only meaningful with a ladder; supplying one is what
			// makes this strategy's minimal valid rule.
			config += "    stages:\n      - name: primary\n        mode: first\n        accounts: [auth-1]\n"
		}
		if err := plugin.Reconfigure([]byte(config)); err != nil {
			t.Fatalf("advertised strategy %q rejected by Reconfigure: %v", strategy, err)
		}
	}
}

func TestManagementRegistrationAdvertisesPanelAndRoutes(t *testing.T) {
	reg := managementRegistration()

	var sawPanel bool
	for _, resource := range reg.Resources {
		if resource.Path == "/panel" {
			sawPanel = true
			if strings.TrimSpace(resource.Menu) == "" {
				t.Fatal("panel resource has no menu label, so the host cannot list it")
			}
		}
	}
	if !sawPanel {
		t.Fatalf("Resources = %+v, want a /panel route", reg.Resources)
	}

	paths := map[string]bool{}
	for _, route := range reg.Routes {
		paths[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		http.MethodGet + " /plugins/" + pluginID + "/status",
		http.MethodPost + " /plugins/" + pluginID + "/preview",
	} {
		if !paths[want] {
			t.Fatalf("Routes missing %q; got %+v", want, reg.Routes)
		}
	}
}

// The host injects both prefixes at registration time; the panel and the routes
// have to follow them rather than assuming the historical defaults.
func TestHostInjectedBasePathsAreUsed(t *testing.T) {
	originalManagement, originalResource := managementBasePath(), resourceBasePath()
	defer func() {
		setManagementBasePath(originalManagement)
		setResourceBasePath(originalResource)
	}()

	raw, err := json.Marshal(pluginapi.ManagementRegistrationRequest{
		BasePath:         "/custom/manage",
		ResourceBasePath: "/custom/resource/staged-scheduler",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := handleMethod("management.register", raw); err != nil {
		t.Fatalf("handleMethod(management.register) error = %v", err)
	}

	if got := managementBasePath(); got != "/custom/manage" {
		t.Fatalf("managementBasePath() = %q, want /custom/manage", got)
	}
	if got := resourceBasePath(); got != "/custom/resource/staged-scheduler" {
		t.Fatalf("resourceBasePath() = %q, want /custom/resource/staged-scheduler", got)
	}
}

// resourceBasePath has to fall back to the plugin ID when the host sends nothing,
// otherwise a legacy host would serve the panel under an empty prefix.
func TestResourceBasePathFallsBackToPluginID(t *testing.T) {
	original := resourceBasePath()
	defer setResourceBasePath(original)

	setResourceBasePath("")
	if got, want := resourceBasePath(), "/v0/resource/plugins/"+pluginID; got != want {
		t.Fatalf("resourceBasePath() = %q, want %q", got, want)
	}
}

func TestStatusPayloadReflectsConfiguredRules(t *testing.T) {
	swapScheduler(t, []byte(`
rules:
  zeta:
    strategy: round-robin
  alpha:
    strategy: provider-weighted-round-robin
    provider-weights:
      provider-a: 2
`))

	payload := statusPayload()
	rules, ok := payload["rules"].([]ruleSnapshot)
	if !ok {
		t.Fatalf("payload[rules] = %T, want []ruleSnapshot", payload["rules"])
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %+v, want 2 entries", rules)
	}
	// Sorted by model ID so the panel renders a stable order.
	if rules[0].Model != "alpha" || rules[1].Model != "zeta" {
		t.Fatalf("rules order = %q, %q; want alpha, zeta", rules[0].Model, rules[1].Model)
	}
	if rules[0].ProviderWeights["provider-a"] != 2 {
		t.Fatalf("alpha weights = %+v, want provider-a:2", rules[0].ProviderWeights)
	}
	if payload["configured"] != true {
		t.Fatalf("configured = %v, want true", payload["configured"])
	}
}

func TestStatusPayloadReportsEmptyConfigurationPlainly(t *testing.T) {
	swapScheduler(t, []byte("rules: {}\n"))

	payload := statusPayload()
	if payload["configured"] != false {
		t.Fatalf("configured = %v, want false", payload["configured"])
	}
	if rules, _ := payload["rules"].([]ruleSnapshot); len(rules) != 0 {
		t.Fatalf("rules = %+v, want empty", rules)
	}
}

// The panel must not display a strategy the build cannot honour, and the status
// payload is where it reads them from.
func TestStatusPayloadAdvertisesSupportedStrategies(t *testing.T) {
	swapScheduler(t, []byte("rules: {}\n"))

	payload := statusPayload()
	got, ok := payload["strategies"].([]string)
	if !ok {
		t.Fatalf("payload[strategies] = %T, want []string", payload["strategies"])
	}
	if len(got) != len(supportedStrategies) {
		t.Fatalf("strategies = %v, want %v", got, supportedStrategies)
	}
	for i, strategy := range supportedStrategies {
		if got[i] != strategy {
			t.Fatalf("strategies[%d] = %q, want %q", i, got[i], strategy)
		}
	}
}

// A probe must not disturb the live cursors: the panel can be opened and replayed
// while real traffic is being scheduled.
func TestProbeDoesNotAdvanceLiveCursors(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  shared:
    strategy: provider-weighted-round-robin
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "alpha-1", Provider: "alpha"},
		{ID: "beta-1", Provider: "beta"},
	}

	// One live pick establishes a cursor position, then the probe replays the
	// same rule. The next live pick must still follow the live sequence.
	first := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "shared", Candidates: candidates})
	if !first.Handled {
		t.Fatalf("live pick = %+v, want handled", first)
	}
	plugin.probe("shared", "", candidates, 8)

	second := plugin.Pick(pluginapi.SchedulerPickRequest{Model: "shared", Candidates: candidates})
	if first.AuthID == "alpha-1" && second.AuthID != "beta-1" {
		t.Fatalf("live picks = %q then %q; probe disturbed the live cursor", first.AuthID, second.AuthID)
	}
	if first.AuthID == "beta-1" && second.AuthID != "alpha-1" {
		t.Fatalf("live picks = %q then %q; probe disturbed the live cursor", first.AuthID, second.AuthID)
	}
}

// Replaying a rule whose every candidate is excluded by weight must report "not
// handled" rather than falling back to an arbitrary credential.
func TestProbeReportsUnhandledWhenEveryCandidateIsExcluded(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  wanted:
    strategy: provider-weighted-round-robin
    provider-weights:
      alpha: 0
      beta: 0
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	steps := plugin.probe("wanted", "", []pluginapi.SchedulerAuthCandidate{
		{ID: "alpha-1", Provider: "alpha"},
		{ID: "beta-1", Provider: "beta"},
	}, 3)

	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	for _, step := range steps {
		if step.Handled || step.AuthID != "" {
			t.Fatalf("step = %+v, want unhandled with no pick", step)
		}
	}
}

// An unlisted provider keeps weight 1, so a rule that only names one provider
// still selects credentials from the others. The panel and the docs both rely on
// this, so it is pinned here rather than left to the implementation to change
// quietly.
func TestProbeDefaultsUnlistedProviderWeightToOne(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  wanted:
    strategy: provider-weighted-round-robin
    provider-weights:
      alpha: 1
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	steps := plugin.probe("wanted", "", []pluginapi.SchedulerAuthCandidate{
		{ID: "beta-1", Provider: "beta"},
	}, 2)

	for _, step := range steps {
		if !step.Handled || step.AuthID != "beta-1" {
			t.Fatalf("step = %+v, want the unlisted provider to remain selectable", step)
		}
	}
}

// The strategy override is what lets the panel try something before saving it.
func TestProbeHonoursStrategyOverride(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte(`
rules:
  target:
    strategy: provider-weighted-round-robin
`)); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	steps := plugin.probe("target", strategyFillFirst, nil, 2)
	for _, step := range steps {
		if step.DelegateBuiltin != strategyFillFirst {
			t.Fatalf("step = %+v, want delegation to %q", step, strategyFillFirst)
		}
	}
}

// Replaying an unknown model with no override must not silently succeed: the
// panel uses this to say "this model has no rule yet".
func TestProbeOnUnknownModelIsUnhandled(t *testing.T) {
	plugin := newSchedulerPlugin()
	if err := plugin.Reconfigure([]byte("rules: {}\n")); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}

	steps := plugin.probe("never-configured", "", []pluginapi.SchedulerAuthCandidate{
		{ID: "alpha-1", Provider: "alpha"},
	}, 2)

	for _, step := range steps {
		if step.Handled {
			t.Fatalf("step = %+v, want unhandled for an unconfigured model", step)
		}
	}
}

func TestHandlePreviewRejectsIncompleteRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "missing model", body: `{"candidates":[{"ID":"a"}]}`, want: "model_required"},
		{name: "missing candidates", body: `{"model":"m"}`, want: "candidates_required"},
		{name: "blank model", body: `{"model":"   ","candidates":[{"ID":"a"}]}`, want: "model_required"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := handlePreview([]byte(test.body))
			if err != nil {
				t.Fatalf("handlePreview() error = %v", err)
			}
			resp := managementResult(t, raw)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("StatusCode = %d, want 400", resp.StatusCode)
			}
			if !strings.Contains(string(resp.Body), test.want) {
				t.Fatalf("body = %s, want it to mention %q", resp.Body, test.want)
			}
		})
	}
}

// A misspelled strategy override must be rejected rather than replayed as "the
// scheduler declined to handle this", which would look like a real result.
func TestHandlePreviewRejectsUnknownStrategyOverride(t *testing.T) {
	swapScheduler(t, []byte(`
rules:
  target:
    strategy: round-robin
`))

	body := map[string]any{
		"model":      "target",
		"strategy":   "fill-frist",
		"candidates": []map[string]any{{"ID": "alpha-1", "Provider": "alpha"}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	out, err := handlePreview(raw)
	if err != nil {
		t.Fatalf("handlePreview() error = %v", err)
	}
	resp := managementResult(t, out)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400 for an unknown strategy", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "unsupported_strategy") {
		t.Fatalf("body = %s, want unsupported_strategy", resp.Body)
	}
}

// The iteration cap belongs in the handler, so a hand written request cannot make
// the plugin build an unbounded replay.
func TestHandlePreviewCapsIterations(t *testing.T) {
	swapScheduler(t, []byte(`
rules:
  capped:
    strategy: round-robin
`))

	body := map[string]any{
		"model":      "capped",
		"iterations": 5000,
		"candidates": []map[string]any{{"ID": "alpha-1", "Provider": "alpha"}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	out, err := handlePreview(raw)
	if err != nil {
		t.Fatalf("handlePreview() error = %v", err)
	}
	resp := managementResult(t, out)
	if !strings.Contains(string(resp.Body), `"iterations":200`) {
		t.Fatalf("response does not report the capped iteration count: %s", resp.Body)
	}
}

// statusPayload and the panel both read the live plugin, so the routes have to be
// reachable through the same dispatcher the host uses.
func TestHandleManagementServesStatusRoute(t *testing.T) {
	swapScheduler(t, []byte(`
rules:
  routed:
    strategy: round-robin
`))

	originalManagement := managementBasePath()
	defer setManagementBasePath(originalManagement)
	setManagementBasePath("/v0/management")

	body, err := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + pluginID + "/status",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(body)
	if err != nil {
		t.Fatalf("handleManagement() error = %v", err)
	}
	resp := managementResult(t, out)
	if !strings.Contains(string(resp.Body), "routed") {
		t.Fatalf("status response does not mention the configured model: %s", resp.Body)
	}
}

func TestHandleManagementServesPanelResource(t *testing.T) {
	originalResource := resourceBasePath()
	defer setResourceBasePath(originalResource)
	setResourceBasePath("/v0/resource/plugins/" + pluginID)

	body, err := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/" + pluginID + "/panel",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(body)
	if err != nil {
		t.Fatalf("handleManagement() error = %v", err)
	}
	resp := managementResult(t, out)

	// The placeholder must be substituted, otherwise the panel would call an
	// invalid management base path.
	html := string(resp.Body)
	if strings.Contains(html, "__SS_MANAGEMENT_BASE_PATH_JSON__") {
		t.Fatal("panel HTML still contains the unsubstituted base path placeholder")
	}
	if got := resp.Headers.Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	if !strings.Contains(html, `const MANAGEMENT_BASE_PATH = "/v0/resource/plugins/`+pluginID) {
		// The injected value is the *management* base path, not the resource
		// one; assert the panel got a real path rather than the placeholder.
		if !strings.Contains(html, `const MANAGEMENT_BASE_PATH = `) {
			t.Fatal("panel HTML does not define MANAGEMENT_BASE_PATH")
		}
	}
}

func TestUnknownManagementPathIsNotFound(t *testing.T) {
	body, err := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + pluginID + "/nope",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := handleManagement(body)
	if err != nil {
		t.Fatalf("handleManagement() error = %v", err)
	}
	resp := managementResult(t, out)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusCode = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "not_found") {
		t.Fatalf("body = %s, want not_found", resp.Body)
	}
}

// managementResult decodes the plugin envelope into the response the host would
// forward, so tests assert on the status and body a browser actually receives.
// Decoding into pluginapi.ManagementResponse also undoes the base64 encoding JSON
// applies to the []byte body on the wire.
func managementResult(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
		Error  *rpcError                    `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, raw)
	}
	if env.Error != nil {
		t.Fatalf("envelope carries an error: %+v", env.Error)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	return env.Result
}

// swapScheduler points the package-level plugin at a fresh instance configured
// with config, and restores the previous one when the test finishes.
func swapScheduler(t *testing.T, config []byte) {
	t.Helper()
	previous := activeScheduler
	t.Cleanup(func() { activeScheduler = previous })

	fresh := newSchedulerPlugin()
	if err := fresh.Reconfigure(config); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	activeScheduler = fresh
}
