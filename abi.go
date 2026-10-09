package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method)
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Model Policy Scheduler",
			Version:          "0.1.0",
			Author:           "bfSan",
			GitHubRepository: "https://github.com/bfSan/cpa-plugin-policy-scheduler",
			ConfigFields: []pluginapi.ConfigField{{
				Name:        "rules",
				Type:        pluginapi.ConfigFieldTypeObject,
				Description: "Exact model IDs mapped to scheduler strategy and optional provider weights.",
			}},
		},
		Capabilities: registrationCapability{Scheduler: true},
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
