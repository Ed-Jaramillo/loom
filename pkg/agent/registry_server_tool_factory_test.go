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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	loomv1 "github.com/teradata-labs/loom/gen/go/loom/v1"
	"github.com/teradata-labs/loom/pkg/shuttle"
)

// serverBuiltStubTool stands in for a tool only the server can construct
// (phoenix_list_traces needs sentinel.phoenix config and an API key).
type serverBuiltStubTool struct{ name string }

func (s *serverBuiltStubTool) Name() string        { return s.name }
func (s *serverBuiltStubTool) Description() string { return "server-built stub" }
func (s *serverBuiltStubTool) InputSchema() *shuttle.JSONSchema {
	return &shuttle.JSONSchema{Type: "object"}
}
func (s *serverBuiltStubTool) Backend() string { return "" }
func (s *serverBuiltStubTool) Execute(context.Context, map[string]interface{}) (*shuttle.Result, error) {
	return &shuttle.Result{Success: true}, nil
}

// TestRegistry_CreateAgent_ServerToolFactory pins that registry-built agents
// (workflow stages) receive tools.builtin entries only the server can build.
// Without the factory, a trace-triage stage declaring phoenix_list_traces ran
// with zero tools and wrote fake tool calls until it hit max_tokens.
func TestRegistry_CreateAgent_ServerToolFactory(t *testing.T) {
	tests := []struct {
		name    string
		factory ServerToolFactory
		want    []string
	}{
		{name: "no factory drops server-built tool", factory: nil, want: []string{}},
		{
			name: "factory builds declared tool",
			factory: func(n string) (shuttle.Tool, error) {
				if n == "phoenix_list_traces" {
					return &serverBuiltStubTool{name: n}, nil
				}
				return nil, nil
			},
			want: []string{"phoenix_list_traces"},
		},
		{
			name:    "factory error skips tool",
			factory: func(string) (shuttle.Tool, error) { return nil, errors.New("PHOENIX_API_KEY is not set") },
			want:    []string{},
		},
		{
			name:    "factory declining a name leaves ByName tools alone",
			factory: func(string) (shuttle.Tool, error) { return nil, nil },
			want:    []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, _ := createTestRegistry(t)
			registry.SetServerToolFactory(tt.factory)

			cfg := createTestAgentConfig("scout")
			cfg.Tools = &loomv1.ToolsConfig{Builtin: []string{"phoenix_list_traces"}}
			registry.RegisterConfig(cfg)

			ag, err := registry.CreateAgent(context.Background(), "scout")
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.want, ag.ListTools())
		})
	}
}

// TestRegistry_CreateAgent_ServerToolFactoryKeepsByNameTools pins that the
// factory is consulted only for names builtin.ByName cannot build.
func TestRegistry_CreateAgent_ServerToolFactoryKeepsByNameTools(t *testing.T) {
	registry, _ := createTestRegistry(t)
	var asked []string
	registry.SetServerToolFactory(func(n string) (shuttle.Tool, error) {
		asked = append(asked, n)
		if n == "phoenix_list_spans" {
			return &serverBuiltStubTool{name: n}, nil
		}
		return nil, nil
	})

	cfg := createTestAgentConfig("writer")
	cfg.Tools = &loomv1.ToolsConfig{Builtin: []string{"file_write", "phoenix_list_spans"}}
	registry.RegisterConfig(cfg)

	ag, err := registry.CreateAgent(context.Background(), "writer")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"file_write", "phoenix_list_spans"}, ag.ListTools())
	assert.Equal(t, []string{"phoenix_list_spans"}, asked)
}
