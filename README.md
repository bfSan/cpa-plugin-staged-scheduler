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

### When the ladder runs dry

`unlisted` answers one question: the ladder named some accounts and not one of them is
available, so what happens? It is the difference between "a ladder is a preference" and "a
ladder is a whitelist".

```yaml
rules:
  my-model:
    strategy: staged
    unlisted: strict      # default
    stages:
      - name: primary
        mode: first
        accounts: [A]
      - name: pool
        mode: weighted-round-robin
        accounts: [C, D, E]
```

| `unlisted` | When none of the named accounts is available |
| --- | --- |
| `strict` (default) | refuse the pick |
| `exclude` | hand back to the host's built-in selector |

Either way only the accounts a stage names are ever *picked*. That is not the setting's
job: it comes from the ladder being the pool. `unlisted` decides the last resort only.

`strict` exists because `exclude` alone did not hold the line. Handing back is not a
neutral no-op: the built-in selector then chooses from the same candidate pool, so it may
pick exactly the credential the operator never named — and with `session-affinity: true`
in `routing:` it chooses from every priority tier rather than just the top one. A ladder
written as a list of allowed accounts therefore was not one. `strict` closes that by
refusing instead.

The refusal is terminal rather than a retry: the rejection carries no HTTP status, so it
is not in the host's retryable set (`403/408/429/500/502/503/504`) and selection stops
instead of looping. The caller sees `auth_unavailable`.

`strict` also refuses when the candidate set is empty, which is the same decision point:
none of the named accounts can serve the model. Treating it as a hand-back instead would
let the host bypass the whitelist simply by offering fewer candidates.

One consequence is worth planning for. `strict` cannot tell "my accounts are all cooling
down" apart from "my accounts are mostly disabled, so the host never offered them". Both
refuse, so a ladder listing credentials that are largely `disabled` will fail requests
that `exclude` would have served from elsewhere. Check the account statuses in the panel's
left column before choosing `strict`, and prefer `exclude` where a fallback matters more
than the boundary.

### An invalid rule disables the whole plugin

A rejected value is not scoped to the model it was written on. The host accepts the PATCH
and writes it to `config.yaml` first; the plugin then validates during reconfiguration and
refuses to register at all. The visible result is that the plugin's routes disappear and
its `status` endpoint starts returning 404:

```
PATCH /v0/management/plugins/staged-scheduler/config   -> {"status":"ok"}
GET   /v0/management/plugins                           -> registered: false
                                                          effective_enabled: false
```

Traffic does not stop, which is what makes this easy to miss: every model the plugin had a
rule for silently falls back to whatever scheduler is next in line. Because the invalid
value stays in `config.yaml`, the plugin stays unregistered across restarts until it is
removed or corrected — so the fix is to PATCH a valid rule back (`{"rules":{}}` clears it),
not to restart.

The panel therefore only offers values the plugin accepts. Anything typed by hand into
`config.yaml` can still hit this, and `make verify` covers the rejection paths.

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
