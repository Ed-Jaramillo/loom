// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

//go:build fts5

package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/teradata-labs/loom/pkg/shuttle"
)

func TestWithoutToolsKeepsGraphMemoryButHidesAllTools(t *testing.T) {
	store := newTestGraphStore(t)
	gmCfg := DefaultGraphMemoryConfig()
	gmCfg.Enabled = true
	gmCfg.EnableExtraction = true

	ag := NewAgent(nil, nil, WithGraphMemoryStore(store, gmCfg), WithoutTools())
	assert.Same(t, store, ag.graphMemoryStore)
	assert.True(t, ag.graphMemoryConfig.Enabled)
	assert.True(t, ag.enableGraphMemoryExtraction)
	assert.Zero(t, ag.ToolCount())
	assert.Empty(t, ag.ListTools())

	ag.RegisterTool(mockTool("dynamic"))
	ag.RegisterTools(mockTool("batch"))
	ag.RegisterLazyTools([]shuttle.Tool{mockTool("lazy")}, alwaysTrigger)
	ag.evaluateLazyTools("trigger")
	ag.checkAndRegisterGraphMemoryTool()
	ag.checkAndRegisterManageSkillsTool()
	ag.checkAndRegisterLoadPatternTool()
	assert.Zero(t, ag.ToolCount(), "later registrations must not restore tools")

	ordinary := NewAgent(nil, nil, WithGraphMemoryStore(store, gmCfg))
	require.Positive(t, ordinary.ToolCount())
	assert.Contains(t, ordinary.ListTools(), "graph_memory")
	ordinary.RegisterTool(mockTool("dynamic"))
	assert.Contains(t, ordinary.ListTools(), "dynamic")
}
