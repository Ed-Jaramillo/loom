// Copyright © 2026 Teradata Corporation - All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loomv1 "github.com/teradata-labs/loom/gen/go/loom/v1"
	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/llm"
	"github.com/teradata-labs/loom/pkg/llm/factory"
	"github.com/teradata-labs/loom/pkg/observability"
	llmtypes "github.com/teradata-labs/loom/pkg/types"
)

// A model installed by SwitchModel must still produce llm.completion spans,
// like the providers startup and hot reload build.
func TestSwitchModel_SwitchedProviderIsTraced(t *testing.T) {
	ag := agent.NewAgent(&mockBackend{}, &countingLLM{name: "bedrock", model: "claude"})
	srv := NewMultiAgentServer(map[string]*agent.Agent{"dev": ag}, nil)
	tracer := observability.NewMockTracer()
	srv.SetTracer(tracer)
	pooled := &countingLLM{name: "bedrock", model: "gpt-6-astra"}
	srv.SetProviderPool(map[string]agent.LLMProvider{"astra": pooled}, "")

	t.Run("pool switch", func(t *testing.T) {
		resp, err := srv.SwitchModel(context.Background(), &loomv1.SwitchModelRequest{AgentId: ag.GetID(), ProviderName: "astra"})
		require.NoError(t, err)
		require.True(t, resp.Success)

		p := ag.GetLLMForRole(loomv1.LLMRole_LLM_ROLE_UNSPECIFIED)
		_, wrapped := p.(*llm.InstrumentedProvider)
		require.True(t, wrapped, "the switched provider is instrumented")

		_, err = p.Chat(context.Background(), []llmtypes.Message{{Role: "user", Content: "hi"}}, nil)
		require.NoError(t, err)
		span := tracer.GetSpanByName("llm.completion")
		require.NotNil(t, span, "a call on the switched model is traced")
		assert.Equal(t, int64(1), pooled.calls.Load())
	})

	t.Run("provider and model switch", func(t *testing.T) {
		t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-token-not-real")
		srv.SetProviderFactory(factory.NewProviderFactory(factory.FactoryConfig{DefaultProvider: "bedrock", BedrockRegion: "us-west-2"}))
		resp, err := srv.SwitchModel(context.Background(), &loomv1.SwitchModelRequest{
			AgentId: ag.GetID(), SessionId: "s1", Provider: "bedrock", Model: "us.openai.gpt-6-astra",
		})
		require.NoError(t, err)
		require.True(t, resp.Success)
		assert.Equal(t, "us.openai.gpt-6-astra", resp.NewModel.Id)

		_, wrapped := ag.GetLLMForRole(loomv1.LLMRole_LLM_ROLE_UNSPECIFIED).(*llm.InstrumentedProvider)
		assert.True(t, wrapped, "the factory-built provider is instrumented")
	})
}

func TestInstrumentSwitchedProvider_NoTracerLeavesProvider(t *testing.T) {
	p := &countingLLM{name: "x", model: "y"}
	assert.Same(t, agent.LLMProvider(p), instrumentSwitchedProvider(p, nil, loomv1.LLMRole_LLM_ROLE_AGENT))
}
