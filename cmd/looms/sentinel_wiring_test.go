// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/sentinel/assistant"
)

func enabledSentinelConfig() *Config {
	cfg := &Config{}
	cfg.Server.Port = 60051
	cfg.Sentinel.Phoenix.BaseURL = "http://phoenix.test:6006"
	cfg.Sentinel.Phoenix.Project = "default"
	cfg.Sentinel.Phoenix.APIKeyEnv = "LOOM_TEST_PHOENIX_KEY"
	cfg.Sentinel.Phoenix.RequestTimeout = "15s"
	cfg.Sentinel.Phoenix.Assistant.Enabled = true
	cfg.Sentinel.Phoenix.Assistant.Model.ProviderType = "builtin"
	cfg.Sentinel.Phoenix.Assistant.Model.Provider = "ANTHROPIC"
	cfg.Sentinel.Phoenix.Assistant.Model.ModelName = "claude-test"
	cfg.Sentinel.Phoenix.Assistant.Timeout = "120s"
	return cfg
}

func TestSentinelConfig_Validate(t *testing.T) {
	t.Run("disabled block needs nothing", func(t *testing.T) {
		var s SentinelConfig
		require.NoError(t, s.Validate())
	})
	t.Run("enabled needs base_url, project, key env and model", func(t *testing.T) {
		cfg := enabledSentinelConfig()
		require.NoError(t, cfg.Sentinel.Validate())

		c := *cfg
		c.Sentinel.Phoenix.BaseURL = ""
		assert.ErrorContains(t, c.Sentinel.Validate(), "base_url")

		c = *cfg
		c.Sentinel.Phoenix.Project = ""
		assert.ErrorContains(t, c.Sentinel.Validate(), "project")

		c = *cfg
		c.Sentinel.Phoenix.APIKeyEnv = ""
		assert.ErrorContains(t, c.Sentinel.Validate(), "api_key_env")

		c = *cfg
		c.Sentinel.Phoenix.Assistant.Model.ModelName = ""
		assert.ErrorContains(t, c.Sentinel.Validate(), "model_name")

		c = *cfg
		c.Sentinel.Phoenix.Assistant.Model.ProviderType = "custom"
		assert.ErrorContains(t, c.Sentinel.Validate(), "provider_id")

		c = *cfg
		c.Sentinel.Phoenix.Assistant.Model.ProviderType = "weird"
		assert.ErrorContains(t, c.Sentinel.Validate(), "provider_type")
	})
	t.Run("durations are checked even when disabled", func(t *testing.T) {
		var s SentinelConfig
		s.Phoenix.RequestTimeout = "soon"
		assert.ErrorContains(t, s.Validate(), "request_timeout")
		s = SentinelConfig{}
		s.Phoenix.Assistant.Timeout = "later"
		assert.ErrorContains(t, s.Validate(), "assistant.timeout")
	})
}

func TestNewPhoenixAssistantTool(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		_, err := newPhoenixAssistantTool(nil, nil, zap.NewNop())
		require.Error(t, err)
	})
	t.Run("disabled", func(t *testing.T) {
		cfg := enabledSentinelConfig()
		cfg.Sentinel.Phoenix.Assistant.Enabled = false
		_, err := newPhoenixAssistantTool(cfg, nil, zap.NewNop())
		assert.True(t, errors.Is(err, errPhoenixAssistantDisabled))
	})
	t.Run("missing key env", func(t *testing.T) {
		t.Setenv("LOOM_TEST_PHOENIX_KEY", "")
		_, err := newPhoenixAssistantTool(enabledSentinelConfig(), nil, zap.NewNop())
		assert.ErrorContains(t, err, "LOOM_TEST_PHOENIX_KEY")
	})
	t.Run("configured", func(t *testing.T) {
		t.Setenv("LOOM_TEST_PHOENIX_KEY", "secret")
		tool, err := newPhoenixAssistantTool(enabledSentinelConfig(), nil, zap.NewNop())
		require.NoError(t, err)
		assert.Equal(t, assistant.ToolName, tool.Name())
	})
}

func TestRegisterYAMLBuiltinTools_PhoenixAssistant(t *testing.T) {
	withViperMinimalMode(t, false, false)
	t.Setenv("LOOM_TEST_PHOENIX_KEY", "secret")

	prev := config
	t.Cleanup(func() { config = prev })

	t.Run("skipped when the server has no sentinel config", func(t *testing.T) {
		config = nil
		ag := agent.NewAgent(nil, nil)
		registerYAMLBuiltinTools(ag, newCfg(assistant.ToolName, "http_request"), nil, zap.NewNop(), "  ", "agent_management", nil)
		assert.False(t, hasTool(ag, assistant.ToolName))
		assert.True(t, hasTool(ag, "http_request"), "other tools still register")
	})
	t.Run("skipped when disabled", func(t *testing.T) {
		config = enabledSentinelConfig()
		config.Sentinel.Phoenix.Assistant.Enabled = false
		ag := agent.NewAgent(nil, nil)
		registerYAMLBuiltinTools(ag, newCfg(assistant.ToolName), nil, zap.NewNop(), "  ", "agent_management", nil)
		assert.False(t, hasTool(ag, assistant.ToolName))
	})
	t.Run("registered when enabled", func(t *testing.T) {
		config = enabledSentinelConfig()
		ag := agent.NewAgent(nil, nil)
		registerYAMLBuiltinTools(ag, newCfg(assistant.ToolName), nil, zap.NewNop(), "  ", "agent_management", nil)
		assert.True(t, hasTool(ag, assistant.ToolName))
	})
}

func TestRegisterYAMLBuiltinTools_PhoenixReadTools(t *testing.T) {
	withViperMinimalMode(t, false, false)
	prev := config
	t.Cleanup(func() { config = prev })

	t.Run("registered with base_url and key, assistant disabled", func(t *testing.T) {
		t.Setenv("LOOM_TEST_PHOENIX_KEY", "secret")
		config = enabledSentinelConfig()
		config.Sentinel.Phoenix.Assistant.Enabled = false
		ag := agent.NewAgent(nil, nil)
		registerYAMLBuiltinTools(ag, newCfg("phoenix_list_traces", "phoenix_list_spans"), nil, zap.NewNop(), "  ", "agent_management", nil)
		assert.True(t, hasTool(ag, "phoenix_list_traces"))
		assert.True(t, hasTool(ag, "phoenix_list_spans"))
		assert.False(t, hasTool(ag, assistant.ToolName))
	})
	t.Run("skipped without the API key", func(t *testing.T) {
		t.Setenv("LOOM_TEST_PHOENIX_KEY", "")
		config = enabledSentinelConfig()
		ag := agent.NewAgent(nil, nil)
		registerYAMLBuiltinTools(ag, newCfg("phoenix_list_spans", "http_request"), nil, zap.NewNop(), "  ", "agent_management", nil)
		assert.False(t, hasTool(ag, "phoenix_list_spans"))
		assert.True(t, hasTool(ag, "http_request"))
	})
	t.Run("skipped without base_url", func(t *testing.T) {
		t.Setenv("LOOM_TEST_PHOENIX_KEY", "secret")
		config = enabledSentinelConfig()
		config.Sentinel.Phoenix.BaseURL = ""
		ag := agent.NewAgent(nil, nil)
		registerYAMLBuiltinTools(ag, newCfg("phoenix_list_spans"), nil, zap.NewNop(), "  ", "agent_management", nil)
		assert.False(t, hasTool(ag, "phoenix_list_spans"))
	})
}
