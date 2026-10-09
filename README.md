# CLIProxyAPI Staged Account Scheduler

Model-aware scheduler plugin for CLIProxyAPI. It keeps the host scheduler as the default and overrides only exact model IDs declared in plugin configuration.

> Fork of [abdwhb-png/cliproxy-model-policy-scheduler](https://github.com/abdwhb-png/cliproxy-model-policy-scheduler), maintained at
> [bfSan/cpa-plugin-staged-scheduler](https://github.com/bfSan/cpa-plugin-staged-scheduler). The original repository is wired as the
> `upstream` remote; upstream keeps the original design, this fork is where our changes land.
>
> Work in this fork: model-scoped staged account scheduling — exhaust account A, then B, then fall through to a
> weighted pool (C/D/E) — while keeping the existing strategies below.

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
    staged-scheduler:
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

`make build` produces `dist/staged-scheduler.so` for Linux AMD64.

## Panel

The plugin ships a browser panel at `/v0/resource/plugins/staged-scheduler/panel`, listed in the
management UI as **Staged Scheduler**. It shows the credential pool CPA currently knows about, edits
the `rules` configuration, and replays a rule offline to show which account each pick would take.

Nothing in the panel is privileged: the page asks for the management key and then calls CPA's own
endpoints from the browser, so the plugin holds no key of its own. Rules are written through
`PATCH /v0/management/plugins/staged-scheduler/config`, which is CPA's plugin-config endpoint.

The replay runs on a throwaway plugin instance, so probing never advances the cursors live traffic
depends on, and it sends no upstream request.

## Management routes

Both routes are authenticated by CPA itself:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v0/management/plugins/staged-scheduler/status` | Configured rules plus the strategies this build implements. |
| `POST` | `/v0/management/plugins/staged-scheduler/preview` | Replay one rule against a candidate set. Body: `{model, strategy?, iterations?, candidates[]}`. An unknown `strategy` is rejected rather than replayed. |

## Local CLIProxyAPI integration

Mount the built library at `/CLIProxyAPI/plugins/staged-scheduler.so`, enable plugins globally, and configure `plugins.configs.staged-scheduler`.

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
