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
	strategyStaged                     = "staged"
	providerGroupByProvider            = "provider"
	providerGroupByBaseURL             = "base-url"
	maxProviderWeight                  = 1_000_000

	// stageModeFirst exhausts the stage's accounts in the order they are
	// listed: the first one present in the candidate set keeps receiving
	// traffic until the host stops offering it (cooldown, disable, quota
	// error). That is what "burn account A before touching B" means here.
	stageModeFirst = "first"
	// stageModeWeightedRoundRobin spreads picks across the stage's accounts,
	// either evenly or by weight.
	stageModeWeightedRoundRobin = "weighted-round-robin"
	// stageModeRoundRobin is accepted as a synonym for the weighted mode with
	// no weights, so an operator who writes the built-in strategy name is not
	// punished for it.
	stageModeRoundRobin = "round-robin"

	// defaultStageName labels a stage the operator left unnamed. Names are only
	// used for cursor keys and panel display, so an empty name must not fail a
	// configuration the operator otherwise got right.
	defaultStageName = "stage"
)

type stageConfig struct {
	// Name labels the stage. Optional; defaults to its position.
	Name string `yaml:"name"`
	// Mode is how accounts inside this stage are chosen.
	Mode string `yaml:"mode"`
	// Accounts lists credential IDs, matched against the candidate IDs the
	// host supplies (the auth file names, extension included).
	Accounts []string `yaml:"accounts"`
	// Weights optionally weights individual accounts inside this stage.
	Weights map[string]int64 `yaml:"weights"`
}

type ruleConfig struct {
	Strategy        string           `yaml:"strategy"`
	ProviderGroupBy string           `yaml:"provider-group-by"`
	ProviderWeights map[string]int64 `yaml:"provider-weights"`
	// Models lists the other model IDs that share this rule. The map key is
	// always the rule's canonical model, so a single ladder can serve an alias
	// and its upstream name without being written twice.
	Models []string `yaml:"models"`
	// Stages is the staged strategy's account ladder.
	Stages []stageConfig `yaml:"stages"`
	// Unlisted decides what happens to candidates no stage names: "exclude"
	// (default) ignores them, "include" appends them to the final stage.
	Unlisted string `yaml:"unlisted"`
	// sharedFrom records the rule this entry was expanded from, when this model
	// was listed under another rule's `models`. It is never configured and never
	// serialized; the panel uses it to show one row per ladder instead of one
	// row per alias.
	sharedFrom string `yaml:"-"`
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
		case strategyStaged:
			normalizedStages, errStages := normalizeStages(normalizedModel, rule.Stages)
			if errStages != nil {
				return errStages
			}
			rule.Stages = normalizedStages
		default:
			return fmt.Errorf("model %q has unsupported strategy %q", normalizedModel, rule.Strategy)
		}

		rule.Unlisted = strings.ToLower(strings.TrimSpace(rule.Unlisted))
		switch rule.Unlisted {
		case "", unlistedExclude:
			rule.Unlisted = unlistedExclude
		case unlistedInclude:
		default:
			return fmt.Errorf("model %q has unsupported unlisted mode %q", normalizedModel, rule.Unlisted)
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

	if err := expandSharedModels(config.Rules); err != nil {
		return err
	}

	p.mu.Lock()
	p.config = config
	p.groupCurrent = make(map[string]map[string]int64)
	p.credentialCursors = make(map[string]map[string]int)
	p.mu.Unlock()
	return nil
}

// expandSharedModels turns each rule's `models:` list into real rule entries that
// share the rule's ladder. The scheduler looks rules up by the exact string the
// client requested, so an alias and its upstream name are two different keys that
// must both resolve; listing them once under `models` is how an operator avoids
// maintaining the same ladder twice.
//
// Cursors are keyed by model, so the shared entries deliberately get their own
// rotation position: two aliases pointing at one ladder are two traffic streams,
// and merging their cursors would make their combined order depend on which alias
// happened to be called first.
func expandSharedModels(rules map[string]ruleConfig) error {
	type sharedEntry struct {
		owner string
		model string
	}
	var additions []sharedEntry

	for owner, rule := range rules {
		seen := make(map[string]bool, len(rule.Models))
		for _, raw := range rule.Models {
			model := strings.TrimSpace(raw)
			if model == "" {
				return fmt.Errorf("model %q has an empty entry in its models list", owner)
			}
			if model == owner {
				// Naming the rule's own key is redundant, not an error: the
				// panel writes the full set back, which includes it.
				continue
			}
			if seen[model] {
				return fmt.Errorf("model %q lists %q twice", owner, model)
			}
			seen[model] = true
			if _, exists := rules[model]; exists {
				return fmt.Errorf("model %q is both a rule of its own and listed under model %q", model, owner)
			}
			additions = append(additions, sharedEntry{owner: owner, model: model})
		}
	}

	for _, entry := range additions {
		shared := rules[entry.owner]
		shared.Models = nil
		shared.sharedFrom = entry.owner
		rules[entry.model] = shared
	}
	return nil
}

// unlistedExclude and unlistedInclude decide what happens to a candidate no stage
// names. Excluding by default is the safe reading: an operator who lists accounts
// is describing the whole pool, and silently appending an unlisted credential
// would route traffic to an account they never mentioned.
const (
	unlistedExclude = "exclude"
	unlistedInclude = "include"
)

// normalizeStages validates and canonicalizes a staged ladder. Every stage has to
// name at least one account with a usable weight, because a stage that can never
// select anything is a configuration mistake that would otherwise only show up as
// unexplained failover much later.
func normalizeStages(model string, stages []stageConfig) ([]stageConfig, error) {
	if len(stages) == 0 {
		return nil, fmt.Errorf("model %q uses strategy %q but declares no stages", model, strategyStaged)
	}

	out := make([]stageConfig, 0, len(stages))
	seenStageNames := make(map[string]bool, len(stages))
	// An account may appear in one stage only. Repeating it would make the
	// ladder ambiguous: the pick would depend on which stage happened to be
	// reached first, which is exactly what the operator is trying to control.
	seenAccounts := make(map[string]string)

	for index, stage := range stages {
		normalized, errStage := normalizeStage(model, index, stage, seenStageNames, seenAccounts)
		if errStage != nil {
			return nil, errStage
		}
		out = append(out, normalized)
	}
	return out, nil
}

func normalizeStage(
	model string,
	index int,
	stage stageConfig,
	seenStageNames map[string]bool,
	seenAccounts map[string]string,
) (stageConfig, error) {
	name := strings.TrimSpace(stage.Name)
	if name == "" {
		name = fmt.Sprintf("%s-%d", defaultStageName, index+1)
	}
	if seenStageNames[name] {
		return stageConfig{}, fmt.Errorf("model %q has duplicate stage name %q", model, name)
	}
	seenStageNames[name] = true
	stage.Name = name

	// An empty mode means "first": the common case is a ladder of single
	// accounts, and demanding an explicit mode for each would be noise.
	rawMode := strings.ToLower(strings.TrimSpace(stage.Mode))
	switch rawMode {
	case "", stageModeFirst:
		stage.Mode = stageModeFirst
	case stageModeWeightedRoundRobin, stageModeRoundRobin:
		stage.Mode = stageModeWeightedRoundRobin
	default:
		return stageConfig{}, fmt.Errorf("model %q stage %q has unsupported mode %q", model, name, stage.Mode)
	}

	if len(stage.Accounts) == 0 && len(stage.Weights) == 0 {
		return stageConfig{}, fmt.Errorf("model %q stage %q lists no accounts", model, name)
	}

	accounts := make([]string, 0, len(stage.Accounts))
	for _, raw := range stage.Accounts {
		account := strings.TrimSpace(raw)
		if account == "" {
			return stageConfig{}, fmt.Errorf("model %q stage %q has an empty account", model, name)
		}
		if previous, duplicate := seenAccounts[account]; duplicate {
			return stageConfig{}, fmt.Errorf("model %q lists account %q in both stage %q and stage %q", model, account, previous, name)
		}
		seenAccounts[account] = name
		accounts = append(accounts, account)
	}
	stage.Accounts = accounts

	weights := make(map[string]int64, len(stage.Weights))
	if stage.Mode == stageModeFirst && len(stage.Weights) > 0 {
		// Weights describe how to spread picks; a "first" stage always takes
		// the same account, so weights there would be silently inert.
		return stageConfig{}, fmt.Errorf("model %q stage %q sets weights but its mode is %q", model, name, stageModeFirst)
	}
	for account, weight := range stage.Weights {
		trimmed := strings.TrimSpace(account)
		if trimmed == "" {
			return stageConfig{}, fmt.Errorf("model %q stage %q has an empty weight key", model, name)
		}
		if weight < 0 {
			return stageConfig{}, fmt.Errorf("model %q stage %q account %q has negative weight", model, name, trimmed)
		}
		if weight > maxProviderWeight {
			return stageConfig{}, fmt.Errorf("model %q stage %q account %q weight exceeds %d", model, name, trimmed, maxProviderWeight)
		}
		if _, known := seenAccounts[trimmed]; !known {
			// A weight for an account nobody listed is a typo, and silently
			// ignoring it would make the weight look applied when it is not.
			return stageConfig{}, fmt.Errorf("model %q stage %q weights account %q, which it does not list", model, name, trimmed)
		}
		weights[trimmed] = weight
	}
	if len(weights) > 0 {
		stage.Weights = weights
	} else {
		stage.Weights = nil
	}
	return stage, nil
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
	case strategyStaged:
		return p.pickStagedLocked(model, rule, request.Candidates)
	default:
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
}

// pickStagedLocked walks the ladder and takes the first stage that still has a
// usable account. The host has already removed cooling, disabled and
// quota-exhausted credentials from Candidates, so "this account is spent" and
// "advance to the next stage" are the same event: no quota API is needed here.
func (p *schedulerPlugin) pickStagedLocked(
	model string,
	rule ruleConfig,
	candidates []pluginapi.SchedulerAuthCandidate,
) pluginapi.SchedulerPickResponse {
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	available := make(map[string]pluginapi.SchedulerAuthCandidate, len(candidates))
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate.ID)
		if id == "" {
			continue
		}
		if _, exists := available[id]; !exists {
			available[id] = candidate
		}
	}
	if len(available) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	stages := rule.Stages
	includeUnlisted := rule.Unlisted == unlistedInclude
	for index, stage := range stages {
		// Only the final stage absorbs unlisted credentials: that is what
		// "burn A, then B, then spill into everything else" means.
		pool := stagePool(stage, available, includeUnlisted && index == len(stages)-1)
		if len(pool) == 0 {
			continue
		}
		selected := p.selectFromStage(model, stage, pool)
		if selected == "" {
			continue
		}
		return pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
	}

	return pluginapi.SchedulerPickResponse{Handled: false}
}

// stagePool returns the candidates of a stage that are actually available,
// ordered by the stage's account list so "first" means the operator's first.
// includeUnlisted appends the credentials no stage named, sorted by ID so the
// resulting order is stable across calls.
func stagePool(
	stage stageConfig,
	available map[string]pluginapi.SchedulerAuthCandidate,
	includeUnlisted bool,
) []pluginapi.SchedulerAuthCandidate {
	pool := make([]pluginapi.SchedulerAuthCandidate, 0, len(stage.Accounts))
	named := make(map[string]bool, len(stage.Accounts))
	for _, account := range stage.Accounts {
		named[account] = true
		if candidate, ok := available[account]; ok {
			pool = append(pool, candidate)
		}
	}
	if !includeUnlisted {
		return pool
	}
	rest := make([]string, 0, len(available))
	for id := range available {
		if !named[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		pool = append(pool, available[id])
	}
	return pool
}

// selectFromStage picks within one stage. "first" always takes the head of the
// pool, which is what burns an account until the host withdraws it.
func (p *schedulerPlugin) selectFromStage(
	model string,
	stage stageConfig,
	pool []pluginapi.SchedulerAuthCandidate,
) string {
	if stage.Mode == stageModeFirst {
		return pool[0].ID
	}

	// Cursors are keyed per model and stage so two models sharing an account
	// pool do not perturb each other's rotation.
	cursors := p.credentialCursors[model]
	if cursors == nil {
		cursors = make(map[string]int)
		p.credentialCursors[model] = cursors
	}
	if len(stage.Weights) == 0 {
		cursor := cursors[stage.Name]
		selected := pool[cursor%len(pool)]
		cursors[stage.Name] = cursor + 1
		return selected.ID
	}

	weightPrefix := stage.Name + "/"
	current := p.groupCurrent[model]
	if current == nil {
		current = make(map[string]int64)
		p.groupCurrent[model] = current
	}
	active := make(map[string]bool, len(pool))
	var totalWeight int64
	for _, candidate := range pool {
		weight := stageWeight(stage.Weights, candidate.ID)
		if weight <= 0 {
			continue
		}
		key := weightPrefix + candidate.ID
		active[key] = true
		current[key] += weight
		totalWeight += weight
	}
	for key := range current {
		if strings.HasPrefix(key, weightPrefix) && !active[key] {
			delete(current, key)
		}
	}

	selected := ""
	var selectedCurrent int64
	for _, candidate := range pool {
		key := weightPrefix + candidate.ID
		if !active[key] {
			continue
		}
		if selected == "" || current[key] > selectedCurrent {
			selected = candidate.ID
			selectedCurrent = current[key]
		}
	}
	if selected == "" {
		return ""
	}
	current[weightPrefix+selected] -= totalWeight
	return selected
}

func stageWeight(weights map[string]int64, id string) int64 {
	if weight, exists := weights[id]; exists {
		return weight
	}
	return 1
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

// stageSnapshot is the panel-facing view of one stage. Accounts are reported in
// the configured order because that order is the ladder's whole meaning.
type stageSnapshot struct {
	Name     string           `json:"name"`
	Mode     string           `json:"mode"`
	Accounts []string         `json:"accounts,omitempty"`
	Weights  map[string]int64 `json:"weights,omitempty"`
}

// ruleSnapshot is the panel-facing view of one configured rule. It mirrors the
// configuration rather than any live cursor, because the panel exists to answer
// "what is configured" before it answers "what would happen".
type ruleSnapshot struct {
	Model           string           `json:"model"`
	Strategy        string           `json:"strategy"`
	ProviderGroupBy string           `json:"provider_group_by,omitempty"`
	ProviderWeights map[string]int64 `json:"provider_weights,omitempty"`
	Stages          []stageSnapshot  `json:"stages,omitempty"`
	Unlisted        string           `json:"unlisted,omitempty"`
	// Owned reports whether this row is the rule an operator wrote, as opposed
	// to an alias expanded from another rule's models list.
	Owned bool `json:"owned"`
	// SharedWith lists every model ID that resolves to this same ladder. The
	// panel edits one row per ladder, so it needs the whole set to write back.
	SharedWith []string `json:"shared_with,omitempty"`
}

// snapshots returns every configured rule, sorted by model ID so the panel has a
// stable order to render. Aliases expanded from a models list are reported with
// their owner in SharedWith, which is what lets the panel show one editable row
// per ladder instead of one row per alias.
func (p *schedulerPlugin) snapshots() []ruleSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Group the expanded aliases by the rule they came from first, so each row
	// can carry its complete set without a second pass over the rules.
	shared := make(map[string][]string)
	for model, rule := range p.config.Rules {
		if rule.sharedFrom != "" {
			shared[rule.sharedFrom] = append(shared[rule.sharedFrom], model)
		}
	}
	for owner := range shared {
		sort.Strings(shared[owner])
	}

	out := make([]ruleSnapshot, 0, len(p.config.Rules))
	for model, rule := range p.config.Rules {
		if rule.sharedFrom != "" {
			continue
		}
		out = append(out, ruleSnapshot{
			Model:           model,
			Strategy:        rule.Strategy,
			ProviderGroupBy: rule.ProviderGroupBy,
			ProviderWeights: cloneWeights(rule.ProviderWeights),
			Stages:          cloneStages(rule.Stages),
			Unlisted:        rule.Unlisted,
			Owned:           true,
			SharedWith:      shared[model],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func cloneStages(src []stageConfig) []stageSnapshot {
	if len(src) == 0 {
		return nil
	}
	out := make([]stageSnapshot, 0, len(src))
	for _, stage := range src {
		out = append(out, stageSnapshot{
			Name:     stage.Name,
			Mode:     stage.Mode,
			Accounts: append([]string(nil), stage.Accounts...),
			Weights:  cloneWeights(stage.Weights),
		})
	}
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
		// Stages has to be copied too: a probe must not be able to observe a
		// reconfigure midway through, and the probe's own copy must not write
		// back into the live rule.
		rule.Stages = cloneStageConfigs(rule.Stages)
		out.Rules[model] = rule
	}
	return out
}

func cloneStageConfigs(src []stageConfig) []stageConfig {
	if len(src) == 0 {
		return nil
	}
	out := make([]stageConfig, 0, len(src))
	for _, stage := range src {
		stage.Accounts = append([]string(nil), stage.Accounts...)
		stage.Weights = cloneWeights(stage.Weights)
		out = append(out, stage)
	}
	return out
}
