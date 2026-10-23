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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadWorkflowAgents_TemperatureUnsetByDefault pins that the chat agents
// synthesized from a workflow file leave temperature unset (0) unless the file
// sets one. A forced 0.7 made Bedrock reject every coordinator and wf:agent
// call on models that deprecate temperature.
func TestLoadWorkflowAgents_TemperatureUnsetByDefault(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want float32
	}{
		{
			name: "orchestration pipeline",
			yaml: `apiVersion: loom/v1
kind: Workflow
metadata:
  name: temp-pipeline
spec:
  type: pipeline
  stages:
    - agent_id: scout
    - agent_id: writer
`,
			want: 0,
		},
		{
			name: "weaver format without llm block",
			yaml: `name: temp-weaver
system_prompt: Coordinate the workflow.
workflow:
  pattern: pipeline
  agents:
    - name: scout
      role: Find failing traces.
`,
			want: 0,
		},
		{
			name: "weaver format llm block without temperature",
			yaml: `name: temp-weaver-llm
system_prompt: Coordinate the workflow.
llm:
  provider: bedrock
  model: us.anthropic.claude-opus-5-5
workflow:
  pattern: pipeline
  agents:
    - name: scout
      role: Find failing traces.
`,
			want: 0,
		},
		{
			name: "weaver format explicit temperature kept",
			yaml: `name: temp-weaver-explicit
system_prompt: Coordinate the workflow.
llm:
  provider: bedrock
  model: us.anthropic.claude-sonnet-5-5
  temperature: 0.3
workflow:
  pattern: pipeline
  agents:
    - name: scout
      role: Find failing traces.
`,
			want: 0.3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wf.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0600))

			configs, err := LoadWorkflowAgents(path, &mockLLMProvider{})
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(configs), 2, "coordinator plus at least one sub-agent")
			for _, cfg := range configs {
				require.NotNil(t, cfg.Llm, cfg.Name)
				assert.InDelta(t, tt.want, cfg.Llm.Temperature, 0.0001, cfg.Name)
			}
		})
	}
}
