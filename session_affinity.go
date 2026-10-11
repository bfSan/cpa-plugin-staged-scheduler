package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Session affinity for staged rules.
//
// Why this exists: when a scheduler plugin returns an AuthID, the host adopts it
// verbatim and never runs its own SessionAffinitySelector. That is deliberate --
// a plugin that picks accounts is assumed to own the choice -- but it means the
// host's session stickiness silently stops applying to any model this plugin
// handles. A staged rule therefore used to send one conversation to a different
// account on every turn, which is exactly what stickiness is supposed to prevent
// (prompt cache hits, and not re-uploading the same context to a new upstream).
//
// Scope: the binding covers the whole ladder, not one stage. An earlier version
// bound only inside weighted stages, on the theory that a "first" stage already
// pins the head of its list. That theory fails as soon as the head account is
// flaky: the stage keeps dropping it from the candidate set, so traffic spills
// into the next stage, and the session ends up alternating between the two --
// the exact instability stickiness is meant to remove. A session is now pinned
// to whichever account the ladder gave it, wherever in the ladder that was.
//
// Invalidation is the part that has to be right. A binding is a hint, not a
// promise: if the bound account is no longer usable -- cooled down, disabled,
// quota exhausted, or no longer one of the accounts this rule may use -- the
// session is simply re-bound to whatever the ladder picks next. Without that, a
// session whose account went away would be stuck forever.

// maxSessionBindings caps the table so a long-lived process cannot grow it
// without bound. When full, the oldest entries are dropped: a session that has
// not been seen for a while re-binds on its next request, which is the same
// outcome as its binding having expired. The lifetime itself comes from the
// session_affinity_ttl switch, defaulting to the host's own 1h.
const maxSessionBindings = 4096

// sessionBinding records which account a session is currently using.
type sessionBinding struct {
	authID string
	model  string
	// stage is where the account came from, kept for the diagnostic view only.
	// It is deliberately not part of the lookup: a binding stays valid as long
	// as the account is usable, even if the ladder would now reach it by a
	// different stage.
	stage   string
	touched time.Time
}

// sessionKey extracts the client's session identity from the headers the host
// passes to schedulers.
//
// The host derives its own session id from the request body (message hashes) as
// well, but a scheduler never receives the body, so only explicit headers are
// available here. That is a real limit, not an oversight: a client that sends no
// session header cannot be pinned, and for those requests the ladder keeps
// rotating exactly as before. The names below mirror the ones the host itself
// recognises, so a client that is sticky for other models is sticky here too.
func sessionKey(headers http.Header) string {
	if len(headers) == 0 {
		return ""
	}
	// Ordered most-specific first. The first header present wins; a client
	// normally sends exactly one of these.
	for _, name := range []string{
		"X-Claude-Code-Session-Id",
		"X-Session-Id",
		"X-Session-ID",
		"Session-Id",
		"Session_id",
		"X-Conversation-Id",
		"X-Thread-Id",
		"Thread-Id",
		"X-Http-Session-Id",
		"X-Session-Affinity",
		"X-Client-Request-Id",
	} {
		if value := headerValue(headers, name); value != "" {
			return name + "=" + value
		}
	}
	return ""
}

// headerValue reads a header case-insensitively. http.Header.Get already does
// that for canonical keys, but the host may hand over keys spelled exactly as the
// client sent them, so both the canonical lookup and a direct map scan are tried.
func headerValue(headers http.Header, name string) string {
	if value := strings.TrimSpace(headers.Get(name)); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// boundAuth returns the account this session is already using for this model, if
// it is still usable.
//
// "Usable" means two things, and both matter:
//
//   - the host is still offering it (present in candidates), so a cooled-down or
//     disabled account does not keep receiving traffic;
//   - the rule still lists it in one of its stages. Candidates come from the host
//     and may contain accounts the ladder never names; honouring a binding to one
//     of those would quietly break strict mode, which promises that only the
//     listed accounts are ever used.
//
// Returning "" means "no usable binding", and the caller walks the ladder as
// usual. The binding is not deleted here: a later request may find the account
// usable again, and in the meantime the caller overwrites it.
func (p *schedulerPlugin) boundAuth(key, model string, candidates []pluginapi.SchedulerAuthCandidate, ladderAccounts map[string]bool, ttl time.Duration) string {
	if key == "" {
		return ""
	}
	binding, ok := p.sessionBindings[key]
	if !ok {
		return ""
	}
	if binding.model != model {
		return ""
	}
	if time.Since(binding.touched) > ttl {
		delete(p.sessionBindings, key)
		return ""
	}
	if !ladderAccounts[binding.authID] {
		return ""
	}
	for _, candidate := range candidates {
		if candidate.ID == binding.authID {
			binding.touched = time.Now()
			p.sessionBindings[key] = binding
			return binding.authID
		}
	}
	return ""
}

// bindSession records the account chosen for this session, and drops expired or
// excess entries while it is here so the table cannot grow without bound.
func (p *schedulerPlugin) bindSession(key, model, stage, authID string, ttl time.Duration) {
	if key == "" || authID == "" {
		return
	}
	now := time.Now()
	p.sessionBindings[key] = sessionBinding{authID: authID, model: model, stage: stage, touched: now}
	if len(p.sessionBindings) <= maxSessionBindings {
		return
	}
	for existingKey, binding := range p.sessionBindings {
		if now.Sub(binding.touched) > ttl {
			delete(p.sessionBindings, existingKey)
		}
	}
	// Still over budget (every entry is fresh): drop the least recently used
	// until there is room. Re-binding is cheap and correct, so losing the
	// oldest binding is strictly better than unbounded growth.
	for len(p.sessionBindings) > maxSessionBindings {
		oldestKey, oldest := "", now
		for existingKey, binding := range p.sessionBindings {
			if oldestKey == "" || binding.touched.Before(oldest) {
				oldestKey, oldest = existingKey, binding.touched
			}
		}
		if oldestKey == "" {
			break
		}
		delete(p.sessionBindings, oldestKey)
	}
}
