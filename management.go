// management.go implements the plugin's own Management API routes and the
// browser panel resource.
//
// Routes registered under /v0/management are authenticated by CPA itself, so the
// plugin adds no key handling of its own. The panel HTML is served as an
// unauthenticated resource and carries no secrets: it asks the operator for the
// management key and then talks to CPA's own management API from the browser,
// exactly like the sibling plugins do.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pluginID is the plugin's stable identity. It is the .so file name, the key
// under plugins.configs, and the path segment the host uses for both the
// management routes and the browser resource prefix, so the three can never
// drift apart.
const pluginID = "staged-scheduler"

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// pathCache holds the prefixes the host injects at registration time. CPA can
// mount management and resources anywhere, so neither may be hardcoded; the
// historical defaults are only a fallback for older hosts.
var pathCache struct {
	mu       sync.RWMutex
	mana     string
	resource string
}

func setManagementBasePath(p string) {
	pathCache.mu.Lock()
	defer pathCache.mu.Unlock()
	pathCache.mana = strings.TrimRight(p, "/")
}

func setResourceBasePath(p string) {
	pathCache.mu.Lock()
	defer pathCache.mu.Unlock()
	pathCache.resource = strings.TrimRight(p, "/")
}

func managementBasePath() string {
	pathCache.mu.RLock()
	defer pathCache.mu.RUnlock()
	if pathCache.mana != "" {
		return pathCache.mana
	}
	return "/v0/management"
}

func resourceBasePath() string {
	pathCache.mu.RLock()
	defer pathCache.mu.RUnlock()
	if pathCache.resource != "" {
		return pathCache.resource
	}
	return "/v0/resource/plugins/" + pluginID
}

// managementRegistration declares the plugin's own API routes plus the browser
// panel. The status route is what the panel reads first; preview replays a
// candidate rule against a candidate set without touching live cursors.
func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + pluginID
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/status", Description: "Currently configured model rules and the strategies this build accepts."},
			{Method: http.MethodPost, Path: base + "/preview", Description: "Replay one model rule against a candidate set and report which credential each pick would select, without advancing live cursors."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "Staged Scheduler", Description: "Inspect the credential pool, edit per-model scheduler rules, and replay a rule to see which account each request would pick."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	path := strings.TrimRight(req.Path, "/")

	// The browser panel is a static resource and is served ahead of any API
	// handling, because resource requests are not management-authenticated.
	if req.Method == http.MethodGet && strings.HasPrefix(path, resourceBasePath()) {
		return okEnvelope(mgmtHTMLResponse(renderPanel()))
	}

	base := managementBasePath() + "/plugins/" + pluginID
	switch {
	case req.Method == http.MethodGet && path == base+"/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, statusPayload()))
	case req.Method == http.MethodPost && path == base+"/preview":
		return handlePreview(req.Body)
	default:
		return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{
			"error": "not_found",
			"path":  path,
		}))
	}
}

func renderPanel() string {
	return strings.ReplaceAll(panelHTML, "__SS_MANAGEMENT_BASE_PATH_JSON__", strconv.Quote(managementBasePath()))
}

// statusPayload reports what is configured right now. The panel renders rule
// rows straight from this, so an empty rule set has to be reported as empty
// rather than dressed up as a default: this plugin deliberately has no default
// strategy, and pretending otherwise would misstate what the host will do.
func statusPayload() map[string]any {
	// One snapshot serves both fields; taking two would let a concurrent
	// reconfigure make "configured" disagree with the rules it accompanies.
	rules := activeScheduler.snapshots()
	return map[string]any{
		"plugin_id":  pluginID,
		"name":       pluginMetadataName,
		"version":    pluginVersion,
		"strategies": append([]string(nil), supportedStrategies...),
		"rules":      rules,
		"configured": len(rules) > 0,
	}
}

// previewRequest describes a candidate rule to replay. Passing candidates in the
// body rather than reading them from the host keeps preview a pure function: what
// the operator sees is exactly the set the panel showed them.
type previewRequest struct {
	Model string `json:"model"`
	// Strategy optionally overrides the configured strategy for Model, which is
	// what lets the panel try a strategy before anything is saved.
	Strategy string `json:"strategy"`
	// Iterations is how many consecutive picks to simulate.
	Iterations int `json:"iterations"`
	// Candidates is the credential set to choose from.
	Candidates []pluginapi.SchedulerAuthCandidate `json:"candidates"`
}

func handlePreview(body []byte) ([]byte, error) {
	var req previewRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "invalid_body"}))
		}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "model_required"}))
	}
	if len(req.Candidates) == 0 {
		return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "candidates_required"}))
	}

	iterations := req.Iterations
	if iterations <= 0 {
		iterations = 8
	}
	// The panel has no legitimate use for thousands of steps, and the replay
	// builds one string per step, so the cap belongs here rather than in the UI.
	if iterations > 200 {
		iterations = 200
	}

	snapshot := activeScheduler.configSnapshot()
	configured, hasRule := snapshot.Rules[model]
	effective := configured.Strategy
	if override := strings.ToLower(strings.TrimSpace(req.Strategy)); override != "" {
		// A typo must not read as "the scheduler declined to handle this": the
		// replay would then look like a legitimate result. Rejecting it keeps
		// every unhandled step meaningful.
		if !isSupportedStrategy(override) {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{
				"error":      "unsupported_strategy",
				"strategy":   override,
				"strategies": append([]string(nil), supportedStrategies...),
			}))
		}
		effective = override
	}

	steps := activeScheduler.probe(model, effective, req.Candidates, iterations)

	// Counts are aggregated here so every consumer reports the same numbers;
	// the panel only formats them.
	counts := make(map[string]int, len(req.Candidates))
	delegated := ""
	handled := 0
	for _, step := range steps {
		if step.AuthID != "" {
			counts[step.AuthID]++
		}
		if step.DelegateBuiltin != "" {
			delegated = step.DelegateBuiltin
		}
		if step.Handled {
			handled++
		}
	}

	return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{
		"model":        model,
		"configured":   hasRule,
		"strategy":     effective,
		"iterations":   iterations,
		"steps":        steps,
		"counts":       counts,
		"handled":      handled,
		"delegated":    delegated,
		"candidates":   len(req.Candidates),
		"unknown_rule": !hasRule && strings.TrimSpace(req.Strategy) == "",
	}))
}

func mgmtJSONResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"error":"marshal_failed"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func mgmtHTMLResponse(html string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(html),
	}
}
