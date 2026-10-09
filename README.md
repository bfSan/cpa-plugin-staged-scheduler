# CLIProxyAPI Model Policy Scheduler

Model-aware scheduler plugin for CLIProxyAPI. It keeps the host scheduler as the default and overrides only exact model IDs declared in plugin configuration.

> Fork of [abdwhb-png/cliproxy-model-policy-scheduler](https://github.com/abdwhb-png/cliproxy-model-policy-scheduler), maintained at
> [bfSan/cpa-plugin-policy-scheduler](https://github.com/bfSan/cpa-plugin-policy-scheduler). The original repository is wired as the
> `upstream` remote; upstream keeps the original design, this fork is where our changes land.

## Why

CLIProxyAPI routing strategy is global. This plugin allows models with different cost or fairness requirements to use different credential policies without modifying the CLIProxyAPI fork.

## Strategies

- `fill-first`: delegate the matching model to CLIProxyAPI's built-in fill-first scheduler.
- `round-robin`: delegate the matching model to CLIProxyAPI's built-in credential round-robin scheduler.
- `provider-weighted-round-robin`: select configured groups proportionally, then rotate credentials inside the selected group. Groups default to provider IDs and may instead use credential base URLs.

Models without a rule return `Handled: false`, so CLIProxyAPI continues with later plugins or its global scheduler.

## Configuration

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cliproxy-model-policy-scheduler:
      enabled: true
      priority: 100
      rules:
        ox-alpha-free:
          strategy: provider-weighted-round-robin
        another-model:
          strategy: provider-weighted-round-robin
          provider-weights:
            openai-compatible-provider-a: 2
            openai-compatible-provider-b: 1
        muse-spark-1.2-contributor:
          strategy: provider-weighted-round-robin
          provider-group-by: base-url
          provider-weights:
            https://opencode.ai/zen/go/v1: 1
            https://opencode.ai/zen/v1: 1
        sequential-model:
          strategy: fill-first
```

`provider-group-by` accepts `provider` (default) or `base-url`. Base URL mode trims valid absolute URLs, lowercases only scheme and host, and preserves path and query case. A candidate without `base_url` falls back to its lowercase provider ID. `provider-weights` uses the same group-key normalization.

Provider IDs and their weight keys are trimmed and normalized to lowercase. Base URL group keys preserve path and query case. Omitted weights default to `1`; `0` excludes a group from a custom rule. Valid weights range from `0` to `1,000,000`. Negative or larger weights, unknown strategies, and unknown grouping modes are rejected atomically during plugin registration or reconfiguration.

Rule matching is exact after trimming surrounding whitespace. Add another model by adding another entry under `rules`; no plugin rebuild is required.

## Commands

```bash
make verify
make build
```

`make build` produces `dist/cliproxy-model-policy-scheduler.so` for Linux AMD64.

## Local CLIProxyAPI integration

Mount the built library at `/CLIProxyAPI/plugins/cliproxy-model-policy-scheduler.so`, enable plugins globally, and configure `plugins.configs.cliproxy-model-policy-scheduler`.

After startup, request `GET /v0/management/plugins` with the existing management authentication and confirm this plugin reports `registered: true` and `effective_enabled: true`.

## Selection behavior

For `provider-weighted-round-robin`, the plugin:

1. Uses only the highest-priority eligible candidates supplied by the host.
2. Groups candidates by normalized provider ID or base URL according to the rule.
3. Applies smooth weighted round-robin between groups.
4. Applies round-robin between credentials within the selected group.
5. Returns only an auth ID present in the request's `Candidates` list.

Cursor state is process-local and resets on valid plugin reconfiguration or process restart. Multiple CLIProxyAPI replicas do not share cursor state.

## Compatibility

The plugin implements CLIProxyAPI native ABI version 1 with RPC schema version 3 and depends on the public `github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi` contract.
