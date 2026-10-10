package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPluginABI_ReconfigureBaseURLGroupingThenPickCandidates(t *testing.T) {
	activeScheduler = newSchedulerPlugin()
	configRequest, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(`
rules:
  muse:
    strategy: provider-weighted-round-robin
    provider-group-by: base-url
`)})
	if err != nil {
		t.Fatalf("marshal config request: %v", err)
	}
	configureRaw, err := handleMethod(pluginabi.MethodPluginReconfigure, configRequest)
	if err != nil {
		t.Fatalf("handle reconfigure: %v", err)
	}
	var configureEnvelope rpcEnvelope
	if err := json.Unmarshal(configureRaw, &configureEnvelope); err != nil {
		t.Fatalf("unmarshal reconfigure response: %v", err)
	}
	if !configureEnvelope.OK {
		t.Fatalf("reconfigure response = %s, want ok envelope", configureRaw)
	}

	pickRequest, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Model: "muse",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "go-1", Provider: "codex", Attributes: map[string]string{"base_url": "https://go.example/v1"}},
			{ID: "go-2", Provider: "codex", Attributes: map[string]string{"base_url": "https://go.example/v1"}},
			{ID: "zen", Provider: "codex", Attributes: map[string]string{"base_url": "https://zen.example/v1"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal scheduler request: %v", err)
	}

	counts := map[string]int{}
	for range 6 {
		pickRaw, err := handleMethod(pluginabi.MethodSchedulerPick, pickRequest)
		if err != nil {
			t.Fatalf("handle scheduler pick: %v", err)
		}
		var pickEnvelope rpcEnvelope
		if err := json.Unmarshal(pickRaw, &pickEnvelope); err != nil {
			t.Fatalf("unmarshal scheduler response: %v", err)
		}
		var response pluginapi.SchedulerPickResponse
		if err := json.Unmarshal(pickEnvelope.Result, &response); err != nil {
			t.Fatalf("unmarshal scheduler result: %v", err)
		}
		if !pickEnvelope.OK || !response.Handled {
			t.Fatalf("scheduler response = %s, want handled", pickRaw)
		}
		counts[response.AuthID]++
	}
	if counts["go-1"] != 2 || counts["go-2"] != 1 || counts["zen"] != 3 {
		t.Fatalf("ABI picks = %#v, want Go group 3 and Zen group 3", counts)
	}
}

func TestPluginABI_ReconfigureThenPickCandidate(t *testing.T) {
	configRequest, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(`
rules:
  ox-alpha-free:
    strategy: provider-weighted-round-robin
`)})
	if err != nil {
		t.Fatalf("marshal config request: %v", err)
	}
	configureRaw, err := handleMethod(pluginabi.MethodPluginReconfigure, configRequest)
	if err != nil {
		t.Fatalf("handle reconfigure: %v", err)
	}
	var configureEnvelope rpcEnvelope
	if err := json.Unmarshal(configureRaw, &configureEnvelope); err != nil {
		t.Fatalf("unmarshal reconfigure response: %v", err)
	}
	if !configureEnvelope.OK {
		t.Fatalf("reconfigure response = %s, want ok envelope", configureRaw)
	}

	pickRequest, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Model: "ox-alpha-free",
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID:       "auth-1",
			Provider: "provider-a",
			Status:   "active",
		}},
	})
	if err != nil {
		t.Fatalf("marshal scheduler request: %v", err)
	}
	pickRaw, err := handleMethod(pluginabi.MethodSchedulerPick, pickRequest)
	if err != nil {
		t.Fatalf("handle scheduler pick: %v", err)
	}
	var pickEnvelope rpcEnvelope
	if err := json.Unmarshal(pickRaw, &pickEnvelope); err != nil {
		t.Fatalf("unmarshal scheduler response: %v", err)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(pickEnvelope.Result, &response); err != nil {
		t.Fatalf("unmarshal scheduler result: %v", err)
	}
	if !pickEnvelope.OK || !response.Handled || response.AuthID != "auth-1" {
		t.Fatalf("scheduler response = %s, want auth-1 handled", pickRaw)
	}
}
