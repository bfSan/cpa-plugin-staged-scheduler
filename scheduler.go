package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	strategyFillFirst                  = "fill-first"
	strategyRoundRobin                 = "round-robin"
	strategyProviderWeightedRoundRobin = "provider-weighted-round-robin"
	providerGroupByProvider            = "provider"
	providerGroupByBaseURL             = "base-url"
	maxProviderWeight                  = 1_000_000
)

type ruleConfig struct {
	Strategy        string           `yaml:"strategy"`
	ProviderGroupBy string           `yaml:"provider-group-by"`
	ProviderWeights map[string]int64 `yaml:"provider-weights"`
}

type pluginConfig struct {
	Rules map[string]ruleConfig `yaml:"rules"`
}

type schedulerPlugin struct {
	mu                sync.Mutex
	config            pluginConfig
	groupCurrent      map[string]map[string]int64
	credentialCursors map[string]map[string]int
}

func newSchedulerPlugin() *schedulerPlugin {
	return &schedulerPlugin{
		groupCurrent:      make(map[string]map[string]int64),
		credentialCursors: make(map[string]map[string]int),
	}
}

func (p *schedulerPlugin) Reconfigure(raw []byte) error {
	var decoded pluginConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return err
	}

	config := pluginConfig{Rules: make(map[string]ruleConfig, len(decoded.Rules))}
	for model, rule := range decoded.Rules {
		normalizedModel := strings.TrimSpace(model)
		if normalizedModel == "" {
			return fmt.Errorf("model rule ID is required")
		}
		if _, duplicate := config.Rules[normalizedModel]; duplicate {
			return fmt.Errorf("duplicate model rule %q", normalizedModel)
		}

		rule.Strategy = strings.ToLower(strings.TrimSpace(rule.Strategy))
		switch rule.Strategy {
		case strategyFillFirst, strategyRoundRobin, strategyProviderWeightedRoundRobin:
		default:
			return fmt.Errorf("model %q has unsupported strategy %q", normalizedModel, rule.Strategy)
		}

		rule.ProviderGroupBy = strings.ToLower(strings.TrimSpace(rule.ProviderGroupBy))
		switch rule.ProviderGroupBy {
		case "", providerGroupByProvider:
			rule.ProviderGroupBy = providerGroupByProvider
		case providerGroupByBaseURL:
		default:
			return fmt.Errorf("model %q has unsupported provider group %q", normalizedModel, rule.ProviderGroupBy)
		}

		normalizedWeights := make(map[string]int64, len(rule.ProviderWeights))
		for provider, weight := range rule.ProviderWeights {
			normalizedProvider := normalizeGroupKey(rule.ProviderGroupBy, provider)
			if normalizedProvider == "" {
				return fmt.Errorf("model %q has an empty provider weight key", normalizedModel)
			}
			if weight < 0 {
				return fmt.Errorf("model %q provider %q has negative weight", normalizedModel, normalizedProvider)
			}
			if weight > maxProviderWeight {
				return fmt.Errorf("model %q provider %q weight exceeds %d", normalizedModel, normalizedProvider, maxProviderWeight)
			}
			if _, duplicate := normalizedWeights[normalizedProvider]; duplicate {
				return fmt.Errorf("model %q has duplicate provider weight %q", normalizedModel, normalizedProvider)
			}
			normalizedWeights[normalizedProvider] = weight
		}
		rule.ProviderWeights = normalizedWeights
		config.Rules[normalizedModel] = rule
	}

	p.mu.Lock()
	p.config = config
	p.groupCurrent = make(map[string]map[string]int64)
	p.credentialCursors = make(map[string]map[string]int)
	p.mu.Unlock()
	return nil
}

func (p *schedulerPlugin) Pick(request pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	model := strings.TrimSpace(request.Model)

	p.mu.Lock()
	defer p.mu.Unlock()

	rule, matched := p.config.Rules[model]
	if !matched {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	switch rule.Strategy {
	case strategyFillFirst, strategyRoundRobin:
		return pluginapi.SchedulerPickResponse{
			DelegateBuiltin: rule.Strategy,
			Handled:         true,
		}
	case strategyProviderWeightedRoundRobin:
		return p.pickProviderWeightedLocked(model, rule, request.Candidates)
	default:
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
}

func (p *schedulerPlugin) pickProviderWeightedLocked(
	model string,
	rule ruleConfig,
	candidates []pluginapi.SchedulerAuthCandidate,
) pluginapi.SchedulerPickResponse {
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	bestPriority := 0
	hasEligible := false
	for _, candidate := range candidates {
		group := candidateGroup(rule, candidate)
		if !eligibleCandidate(rule, candidate, group) {
			continue
		}
		if !hasEligible || candidate.Priority > bestPriority {
			bestPriority = candidate.Priority
			hasEligible = true
		}
	}
	if !hasEligible {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	byGroup := make(map[string][]pluginapi.SchedulerAuthCandidate)
	for _, candidate := range candidates {
		group := candidateGroup(rule, candidate)
		if candidate.Priority != bestPriority || !eligibleCandidate(rule, candidate, group) {
			continue
		}
		byGroup[group] = append(byGroup[group], candidate)
	}
	if len(byGroup) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	groups := make([]string, 0, len(byGroup))
	for group := range byGroup {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		sort.Slice(byGroup[group], func(i, j int) bool {
			return byGroup[group][i].ID < byGroup[group][j].ID
		})
	}

	current := p.groupCurrent[model]
	if current == nil {
		current = make(map[string]int64)
		p.groupCurrent[model] = current
	}
	for group := range current {
		if _, active := byGroup[group]; !active {
			delete(current, group)
		}
	}

	selectedGroup := ""
	var selectedCurrent int64
	var totalWeight int64
	for _, group := range groups {
		weight := groupWeight(rule, group)
		current[group] += weight
		totalWeight += weight
		if selectedGroup == "" || current[group] > selectedCurrent {
			selectedGroup = group
			selectedCurrent = current[group]
		}
	}
	current[selectedGroup] -= totalWeight

	groupCursors := p.credentialCursors[model]
	if groupCursors == nil {
		groupCursors = make(map[string]int)
		p.credentialCursors[model] = groupCursors
	}
	groupCandidates := byGroup[selectedGroup]
	cursor := groupCursors[selectedGroup] % len(groupCandidates)
	selected := groupCandidates[cursor]
	groupCursors[selectedGroup] = cursor + 1

	return pluginapi.SchedulerPickResponse{AuthID: selected.ID, Handled: true}
}

func candidateGroup(rule ruleConfig, candidate pluginapi.SchedulerAuthCandidate) string {
	provider := normalizeGroupKey(providerGroupByProvider, candidate.Provider)
	if rule.ProviderGroupBy != providerGroupByBaseURL {
		return provider
	}
	baseURL := normalizeGroupKey(providerGroupByBaseURL, candidate.Attributes["base_url"])
	if baseURL != "" {
		return baseURL
	}
	return provider
}

func normalizeGroupKey(groupBy, value string) string {
	trimmed := strings.TrimSpace(value)
	if groupBy != providerGroupByBaseURL {
		return strings.ToLower(trimmed)
	}
	return normalizeBaseURL(trimmed)
}

func normalizeBaseURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String()
}

func eligibleCandidate(
	rule ruleConfig,
	candidate pluginapi.SchedulerAuthCandidate,
	group string,
) bool {
	return group != "" && strings.TrimSpace(candidate.ID) != "" && groupWeight(rule, group) > 0
}

func groupWeight(rule ruleConfig, group string) int64 {
	if weight, exists := rule.ProviderWeights[group]; exists {
		return weight
	}
	return 1
}

// -----------------------------------------------------------------------------
// Panel support: read-only snapshots and offline probing
// -----------------------------------------------------------------------------

// ruleSnapshot is the panel-facing view of one configured rule. It mirrors the
// configuration rather than any live cursor, because the panel exists to answer
// "what is configured" before it answers "what would happen".
type ruleSnapshot struct {
	Model           string           `json:"model"`
	Strategy        string           `json:"strategy"`
	ProviderGroupBy string           `json:"provider_group_by,omitempty"`
	ProviderWeights map[string]int64 `json:"provider_weights,omitempty"`
}

// snapshots returns every configured rule, sorted by model ID so the panel has a
// stable order to render.
func (p *schedulerPlugin) snapshots() []ruleSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ruleSnapshot, 0, len(p.config.Rules))
	for model, rule := range p.config.Rules {
		out = append(out, ruleSnapshot{
			Model:           model,
			Strategy:        rule.Strategy,
			ProviderGroupBy: rule.ProviderGroupBy,
			ProviderWeights: cloneWeights(rule.ProviderWeights),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func cloneWeights(src map[string]int64) map[string]int64 {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]int64, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

// probeStep is one simulated pick. A step is either a concrete credential, a
// delegation back to a built-in scheduler, or a refusal to handle the request.
type probeStep struct {
	Index           int    `json:"index"`
	Handled         bool   `json:"handled"`
	AuthID          string `json:"auth_id,omitempty"`
	DelegateBuiltin string `json:"delegate_builtin,omitempty"`
}

// probe replays a rule against a candidate set on a throwaway instance, so the
// panel can show what the scheduler would pick without advancing the cursors the
// live traffic depends on. strategy optionally overrides the configured one for
// the probed model, which is what makes "try another strategy" possible before
// anything is saved.
func (p *schedulerPlugin) probe(
	model string,
	strategy string,
	candidates []pluginapi.SchedulerAuthCandidate,
	iterations int,
) []probeStep {
	probeCfg := p.configSnapshot()
	trimmedModel := strings.TrimSpace(model)
	if override := strings.ToLower(strings.TrimSpace(strategy)); override != "" {
		rule := probeCfg.Rules[trimmedModel]
		rule.Strategy = override
		probeCfg.Rules[trimmedModel] = rule
	}

	run := newSchedulerPlugin()
	run.config = probeCfg

	steps := make([]probeStep, 0, iterations)
	for index := 0; index < iterations; index++ {
		response := run.Pick(pluginapi.SchedulerPickRequest{
			Model:      trimmedModel,
			Candidates: candidates,
		})
		steps = append(steps, probeStep{
			Index:           index,
			Handled:         response.Handled,
			AuthID:          response.AuthID,
			DelegateBuiltin: response.DelegateBuiltin,
		})
	}
	return steps
}

// configSnapshot copies the current rules so a probe never shares map storage
// with the live configuration.
func (p *schedulerPlugin) configSnapshot() pluginConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := pluginConfig{Rules: make(map[string]ruleConfig, len(p.config.Rules))}
	for model, rule := range p.config.Rules {
		rule.ProviderWeights = cloneWeights(rule.ProviderWeights)
		out.Rules[model] = rule
	}
	return out
}
