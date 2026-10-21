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
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/teradata-labs/loom/internal/version"
	"github.com/teradata-labs/loom/pkg/observability"
	"github.com/teradata-labs/loom/pkg/observability/phoenix"
	"github.com/teradata-labs/loom/pkg/prompts"
	"github.com/teradata-labs/loom/pkg/sentinel/assistant"
	"github.com/teradata-labs/loom/pkg/sentinel/phoenixread"
	"github.com/teradata-labs/loom/pkg/shuttle"
)

// errPhoenixAssistantDisabled is returned when the tool is listed in an
// agent's YAML but sentinel.phoenix.assistant.enabled is false.
var errPhoenixAssistantDisabled = errors.New("sentinel.phoenix.assistant.enabled is false")

// newSentinelPhoenixClient builds the narrow Phoenix REST client from
// sentinel.phoenix. The API key is read from the named environment variable
// and never logged. tracer may be nil.
func newSentinelPhoenixClient(cfg *Config, tracer observability.Tracer) (*phoenix.Client, error) {
	if cfg == nil {
		return nil, errors.New("server config is not loaded")
	}
	p := cfg.Sentinel.Phoenix
	if p.BaseURL == "" {
		return nil, errors.New("sentinel.phoenix.base_url is not set")
	}
	keyEnv := p.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "PHOENIX_API_KEY"
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		return nil, fmt.Errorf("environment variable %s (sentinel.phoenix.api_key_env) is not set", keyEnv)
	}
	var timeout time.Duration
	if p.RequestTimeout != "" {
		d, err := time.ParseDuration(p.RequestTimeout)
		if err != nil {
			return nil, fmt.Errorf("sentinel.phoenix.request_timeout: %w", err)
		}
		timeout = d
	}
	return phoenix.New(phoenix.Config{
		BaseURL:          p.BaseURL,
		APIKey:           key,
		RequestTimeout:   timeout,
		MaxPages:         p.MaxPages,
		MaxResponseBytes: p.MaxResponseBytes,
		Tracer:           tracer,
	})
}

// newPhoenixAssistantTool builds ask_phoenix_assistant from sentinel.phoenix.
// It returns an error, not a tool, when the tool is disabled or cannot be
// configured; the caller logs and skips the YAML mention.
func newPhoenixAssistantTool(cfg *Config, promptRegistry prompts.PromptRegistry, logger *zap.Logger) (shuttle.Tool, error) {
	if cfg == nil {
		return nil, errors.New("server config is not loaded")
	}
	a := cfg.Sentinel.Phoenix.Assistant
	if !a.Enabled {
		return nil, errPhoenixAssistantDisabled
	}
	client, err := newSentinelPhoenixClient(cfg, nil)
	if err != nil {
		return nil, err
	}
	var timeout time.Duration
	if a.Timeout != "" {
		d, err := time.ParseDuration(a.Timeout)
		if err != nil {
			return nil, fmt.Errorf("sentinel.phoenix.assistant.timeout: %w", err)
		}
		timeout = d
	}
	tool, err := assistant.New(client, assistant.Config{
		Project: cfg.Sentinel.Phoenix.Project,
		Model: phoenix.ModelSelection{
			ProviderType: a.Model.ProviderType,
			Provider:     a.Model.Provider,
			ProviderID:   a.Model.ProviderID,
			ModelName:    a.Model.ModelName,
		},
		ApproveTools:      a.ApproveTools,
		Timeout:           timeout,
		MaxApprovalRounds: a.MaxApprovalRounds,
		RecordLocalTraces: a.RecordLocalTraces,
		Prompts:           promptRegistry,
		LoomVersion:       version.Get(),
	})
	if err != nil {
		return nil, err
	}
	if logger != nil {
		logger.Info("ask_phoenix_assistant configured",
			zap.String("phoenix_base_url", cfg.Sentinel.Phoenix.BaseURL),
			zap.String("project", cfg.Sentinel.Phoenix.Project),
			zap.String("model", a.Model.ModelName),
			zap.Strings("approve_tools", a.ApproveTools),
			zap.Bool("record_local_traces", a.RecordLocalTraces))
	}
	return tool, nil
}

// isPhoenixReadTool reports whether name is one of the read-only Phoenix tools.
func isPhoenixReadTool(name string) bool {
	for _, n := range phoenixread.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// newPhoenixReadTool builds a read-only Phoenix tool from sentinel.phoenix.
// It needs base_url and the API key env var, not the assistant switch.
func newPhoenixReadTool(name string, cfg *Config) (shuttle.Tool, error) {
	client, err := newSentinelPhoenixClient(cfg, nil)
	if err != nil {
		return nil, err
	}
	return phoenixread.New(name, client, phoenixread.Config{Project: cfg.Sentinel.Phoenix.Project, MaxValueChars: cfg.Sentinel.Phoenix.ReadMaxValueChars})
}
