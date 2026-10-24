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
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	loomv1 "github.com/teradata-labs/loom/gen/go/loom/v1"
)

const stageWorkflowYAML = `apiVersion: loom/v1
kind: Workflow
metadata:
  name: triage
spec:
  type: pipeline
  stages:
    - agent_id: scout
    - agent_id: quiet
    - agent_id: missing
`

const scoutAgentYAML = `apiVersion: loom/v1
kind: Agent
metadata:
  name: scout
  description: Finds failing traces
spec:
  llm:
    provider: bedrock
    model: us.anthropic.claude-opus-5-5
  system_prompt: Find failing traces in Phoenix.
  tools:
    builtin:
      - phoenix_list_traces
      - send_message
    mcp:
      - server: phoenix
`

const quietAgentYAML = `apiVersion: loom/v1
kind: Agent
metadata:
  name: quiet
spec:
  system_prompt: Answer from context only.
  tools:
    none: true
`

// loadStageWorkflow lays out $LOOM_DATA_DIR/{workflows,agents} and loads the
// workflow the way the registry does.
func loadStageWorkflow(t *testing.T) map[string]*loomv1.AgentConfig {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "agents", "scout.yaml"), []byte(scoutAgentYAML), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "agents", "quiet.yaml"), []byte(quietAgentYAML), 0600))
	wf := filepath.Join(root, "workflows", "triage.yaml")
	require.NoError(t, os.WriteFile(wf, []byte(stageWorkflowYAML), 0600))

	configs, err := LoadWorkflowAgents(wf, &mockLLMProvider{})
	require.NoError(t, err)
	byName := make(map[string]*loomv1.AgentConfig, len(configs))
	for _, c := range configs {
		byName[c.Name] = c
	}
	return byName
}

// TestLoadWorkflowAgents_StageInheritsAgentConfig pins that a chat-mode stage
// agent is the real agent plus the communication tools. As a placeholder,
// trace-triage:phoenix-copilot could see the Phoenix MCP tools in tool_search
// but not call them.
func TestLoadWorkflowAgents_StageInheritsAgentConfig(t *testing.T) {
	stage := loadStageWorkflow(t)["triage:scout"]
	require.NotNil(t, stage)

	assert.True(t, strings.HasPrefix(stage.SystemPrompt, "Find failing traces in Phoenix."), stage.SystemPrompt)
	assert.Contains(t, stage.SystemPrompt, `"triage" workflow`)
	assert.Equal(t, "Finds failing traces", stage.Description)
	require.NotNil(t, stage.Llm)
	assert.Equal(t, "bedrock", stage.Llm.Provider)
	assert.Equal(t, "us.anthropic.claude-opus-5-5", stage.Llm.Model)

	require.NotNil(t, stage.Tools)
	require.Len(t, stage.Tools.Mcp, 1)
	assert.Equal(t, "phoenix", stage.Tools.Mcp[0].Server)
	assert.Equal(t,
		[]string{"phoenix_list_traces", "send_message", "shared_memory_read", "shared_memory_write"},
		stage.Tools.Builtin, "keeps the agent's tools, adds communication tools once")

	assert.Equal(t, "workflow_agent", stage.Metadata["type"])
	assert.Equal(t, "executor", stage.Metadata["role"])
	assert.Equal(t, "scout", stage.Metadata["agent_id"])
	assert.Equal(t, "scout", stage.Metadata["base_agent"])
	assert.Equal(t, "triage", stage.Metadata["workflow"])
}

func TestLoadWorkflowAgents_StageHonorsToolsNone(t *testing.T) {
	stage := loadStageWorkflow(t)["triage:quiet"]
	require.NotNil(t, stage)
	require.NotNil(t, stage.Tools)
	assert.True(t, stage.Tools.None)
	assert.Empty(t, stage.Tools.Builtin, "tools.none is not overridden by communication tools")
}

func TestLoadWorkflowAgents_StageWithoutAgentFileIsPlaceholder(t *testing.T) {
	stage := loadStageWorkflow(t)["triage:missing"]
	require.NotNil(t, stage)
	assert.Contains(t, stage.SystemPrompt, "missing agent in the triage workflow")
	assert.Equal(t, "workflow_agent", stage.Metadata["type"])
	assert.Equal(t, "missing", stage.Metadata["agent_id"])
	assert.NotContains(t, stage.Metadata, "base_agent")
}
