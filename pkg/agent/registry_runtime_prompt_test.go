// Copyright © 2026 Teradata Corporation - All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	loomv1 "github.com/teradata-labs/loom/gen/go/loom/v1"
)

// TestBuildAgent_RuntimePromptMatchesConfig checks the system prompt a
// registry-built agent actually sends, not the proto the server echoes back.
// WithConfig replaces Agent.config wholesale, so any config field an earlier
// option set is lost once a behavior block is present — and every inline
// POST /v1/agents config carries one after ensureConfigDefaults.
func TestBuildAgent_RuntimePromptMatchesConfig(t *testing.T) {
	const (
		systemPrompt = "Score each Mastermind guess against the secret code and report black and white pegs."
		description  = "Mastermind pilot agent"
	)
	// The shape ensureConfigDefaults gives an inline config with no behavior block.
	postBehavior := func() *loomv1.BehaviorConfig {
		return &loomv1.BehaviorConfig{
			MaxIterations:     10,
			TimeoutSeconds:    300,
			MaxTurns:          25,
			MaxToolExecutions: 50,
		}
	}

	tests := []struct {
		name        string
		behavior    *loomv1.BehaviorConfig
		rom         string
		metadata    map[string]string
		wantBaseROM bool
		wantTDROM   bool
	}{
		{
			name:        "behavior block (POST /v1/agents shape)",
			behavior:    postBehavior(),
			wantBaseROM: true,
		},
		{
			name:        "no behavior block",
			behavior:    nil,
			wantBaseROM: true,
		},
		{
			name:     "rom none drops the base ROM",
			behavior: postBehavior(),
			rom:      "none",
		},
		{
			name:        "rom TD adds the Teradata ROM",
			behavior:    postBehavior(),
			rom:         "TD",
			wantBaseROM: true,
			wantTDROM:   true,
		},
		{
			name:        "backend_path metadata auto-detects the Teradata ROM",
			behavior:    postBehavior(),
			metadata:    map[string]string{"backend_path": "backends/teradata-prod.yaml"},
			wantBaseROM: true,
			wantTDROM:   true,
		},
		{
			name:        "rom TD without a behavior block",
			behavior:    nil,
			rom:         "TD",
			wantBaseROM: true,
			wantTDROM:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, _ := createTestRegistry(t)
			ctx := context.Background()

			registry.RegisterConfig(&loomv1.AgentConfig{
				Name:         "runtime-prompt-agent",
				Description:  description,
				SystemPrompt: systemPrompt,
				Rom:          tt.rom,
				Metadata:     tt.metadata,
				Llm:          &loomv1.LLMConfig{},
				Memory:       &loomv1.MemoryConfig{Type: "memory", MaxHistory: 50},
				Behavior:     tt.behavior,
			})
			ag, err := registry.CreateAgent(ctx, "runtime-prompt-agent")
			require.NoError(t, err)

			assert.Equal(t, description, ag.GetDescription())

			prompt := ag.getSystemPrompt(ctx)
			assert.Contains(t, prompt, systemPrompt, "runtime system prompt must carry the configured system_prompt")
			if tt.wantBaseROM {
				assert.Contains(t, prompt, baseROM, "base ROM missing")
			} else {
				assert.NotContains(t, prompt, baseROM, "base ROM present despite rom: none")
			}
			if tt.wantTDROM {
				assert.Contains(t, prompt, teradataROM, "Teradata ROM missing")
			} else {
				assert.NotContains(t, prompt, teradataROM, "Teradata ROM present but not requested")
			}
		})
	}
}

// TestBuildAgent_BehaviorStillApplied guards the other direction: moving the
// identity fields out from under WithConfig must not lose the behavior limits
// or the pattern config WithConfig and WithPatternConfig carry.
func TestBuildAgent_BehaviorStillApplied(t *testing.T) {
	registry, _ := createTestRegistry(t)

	ag, err := registry.buildAgent(context.Background(), &loomv1.AgentConfig{
		Name:         "behavior-agent",
		SystemPrompt: "Summarize the ticket.",
		Llm:          &loomv1.LLMConfig{},
		Behavior: &loomv1.BehaviorConfig{
			MaxToolExecutions:      7,
			MaxTurns:               3,
			OutputTokenCbThreshold: 4,
			Patterns: &loomv1.PatternConfig{
				Enabled:            true,
				MinConfidence:      0.9,
				MaxPatternsPerTurn: 2,
			},
		},
		Metadata: map[string]string{"_enable_self_healing": "false"},
	})
	require.NoError(t, err)

	cfg := ag.GetConfig()
	assert.Equal(t, "behavior-agent", cfg.Name)
	assert.Equal(t, "Summarize the ticket.", cfg.SystemPrompt)
	assert.Equal(t, 7, cfg.MaxToolExecutions)
	assert.Equal(t, 3, cfg.MaxTurns)
	assert.Equal(t, 4, cfg.OutputTokenCBThreshold)
	assert.False(t, cfg.EnableSelfHealing)
	require.NotNil(t, cfg.PatternConfig)
	assert.InDelta(t, 0.9, cfg.PatternConfig.MinConfidence, 0.001)
	assert.Equal(t, 2, cfg.PatternConfig.MaxPatternsPerTurn)
}
