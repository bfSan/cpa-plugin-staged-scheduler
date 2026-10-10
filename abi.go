package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Plugin identity. The name is what the CPA plugin list shows; the repository
// URL is what an operator follows to find the source of a build they did not
// install themselves.
const (
	pluginMetadataName = "Staged Account Scheduler"
	pluginVersion      = "0.4.4"
	pluginAuthor       = "bfSan"
	pluginRepoURL      = "https://github.com/bfSan/cpa-plugin-staged-scheduler"
)

// supportedStrategies is the exact set this build accepts, in the order the panel
// offers them. Validation and the panel both read this list, so a strategy
// cannot be advertised in the UI without being implemented in Pick.
var supportedStrategies = []string{
	strategyFillFirst,
	strategyRoundRobin,
	strategyProviderWeightedRoundRobin,
	strategyStaged,
}

// isSupportedStrategy reports whether Pick implements the named strategy.
func isSupportedStrategy(strategy string) bool {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	for _, candidate := range supportedStrategies {
		if candidate == normalized {
			return true
		}
	}
	return false
}

type rpcEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler bool `json:"scheduler"`
	// SchedulerAcrossPriorities is what makes the staged strategy possible: with
	// it off, the host offers only the highest priority tier, so a ladder could
	// never reach B, C, D or E.
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities,omitempty"`
	ManagementAPI             bool `json:"management_api"`
}

var activeScheduler = newSchedulerPlugin()

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &lifecycle); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		if err := activeScheduler.Reconfigure(lifecycle.ConfigYAML); err != nil {
			return nil, fmt.Errorf("configure scheduler: %w", err)
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		var schedulerRequest pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(request, &schedulerRequest); err != nil {
			return nil, fmt.Errorf("decode scheduler request: %w", err)
		}
		return okEnvelope(activeScheduler.Pick(schedulerRequest))
	case pluginabi.MethodManagementRegister:
		// The host passes the prefixes it wants this plugin to serve under.
		// Capturing them here is what lets the panel call back into the right
		// management base path when CPA mounts it somewhere other than the
		// historical /v0/management.
		var registrationRequest pluginapi.ManagementRegistrationRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &registrationRequest); err == nil {
				if registrationRequest.BasePath != "" {
					setManagementBasePath(registrationRequest.BasePath)
				}
				if registrationRequest.ResourceBasePath != "" {
					setResourceBasePath(registrationRequest.ResourceBasePath)
				}
			}
		}
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method)
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginMetadataName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepoURL,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "rules",
					Type:        pluginapi.ConfigFieldTypeObject,
					Description: "Exact model IDs mapped to a scheduler strategy and its options.",
				},
			},
		},
		Capabilities: registrationCapability{
			Scheduler: true,
			// The staged ladder is defined over accounts that may sit on
			// different priority tiers, so the plugin must see all of them.
			SchedulerAcrossPriorities: true,
			ManagementAPI:             true,
		},
	}
}

func okEnvelope(result any) ([]byte, error) {
	rawResult, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rpcEnvelope{OK: true, Result: rawResult})
}

func errorEnvelope(code, message string) ([]byte, error) {
	return json.Marshal(rpcEnvelope{
		OK:    false,
		Error: &rpcError{Code: code, Message: message},
	})
}
