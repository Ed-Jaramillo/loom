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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loomv1 "github.com/teradata-labs/loom/gen/go/loom/v1"
	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/communication"
	llmtypes "github.com/teradata-labs/loom/pkg/llm/types"
	"github.com/teradata-labs/loom/pkg/session"
	"github.com/teradata-labs/loom/pkg/shuttle"
	"github.com/teradata-labs/loom/pkg/types"
)

// askingLLM asks "probe" a question on the user's turn and records the
// session in which the answer comes back.
type askingLLM struct {
	mu           sync.Mutex
	asked        bool
	reply        string
	replySession string
}

func (m *askingLLM) Chat(ctx context.Context, messages []llmtypes.Message, _ []shuttle.Tool) (*llmtypes.LLMResponse, error) {
	last := messages[len(messages)-1]
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Contains(last.Content, "[MESSAGE FROM ") {
		m.reply = last.Content
		m.replySession = session.SessionIDFromContext(ctx)
		return &llmtypes.LLMResponse{Content: "got it"}, nil
	}
	if last.Role == "user" && !m.asked {
		m.asked = true
		return &llmtypes.LLMResponse{ToolCalls: []types.ToolCall{{
			ID:    "ask-1",
			Name:  "send_message",
			Input: map[string]interface{}{"to_agent": "probe", "message": "how many spans?"},
		}}}, nil
	}
	return &llmtypes.LLMResponse{Content: "asked probe"}, nil
}

func (m *askingLLM) Name() string  { return "asking-llm" }
func (m *askingLLM) Model() string { return "asking-model" }

func (m *askingLLM) got() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reply, m.replySession
}

func standaloneServer(t *testing.T, agents map[string]*agent.Agent) *MultiAgentServer {
	t.Helper()
	srv := setupBroadcastTestServer(t, agents, nil)
	srv.mu.Lock()
	for name, ag := range agents {
		srv.agents[name] = ag
	}
	srv.mu.Unlock()
	monitorCtx, stop := context.WithCancel(context.Background())
	srv.StartMessageQueueMonitor(monitorCtx)
	t.Cleanup(stop)
	return srv
}

// Two standalone agents: dev asks probe, probe answers, and the answer lands
// in the dev session that asked.
func TestStandaloneAgents_RoundTripLandsInAskingSession(t *testing.T) {
	devLLM := &askingLLM{}
	probeLLM := &replyingLLM{}
	backend := &mockBackend{}
	dev := agent.NewAgent(backend, devLLM)
	probe := agent.NewAgent(backend, probeLLM)
	srv := standaloneServer(t, map[string]*agent.Agent{"dev": dev, "probe": probe})

	assert.True(t, srv.messageQueue.HasListener("probe"), "a loaded standalone agent counts as listening")

	devSession := GenerateSessionID()
	_, err := dev.Chat(context.Background(), devSession, "ask probe how many spans")
	require.NoError(t, err)

	require.Eventually(t, func() bool { r, _ := devLLM.got(); return r != "" }, 10*time.Second, 50*time.Millisecond,
		"probe's reply reaches dev")
	assert.Contains(t, probeLLM.message(), "how many spans?", "probe got dev's question")
	reply, replySession := devLLM.got()
	assert.Contains(t, reply, "worker result: 42")
	assert.Equal(t, devSession, replySession, "the reply lands in the session that asked")
}

func TestStandaloneDelivery_HopLimitRejectsWithoutATurn(t *testing.T) {
	probeLLM := &replyingLLM{}
	probe := agent.NewAgent(&mockBackend{}, probeLLM)
	srv := setupBroadcastTestServer(t, map[string]*agent.Agent{"probe": probe}, nil)
	srv.mu.Lock()
	srv.agents["probe"] = probe
	srv.mu.Unlock()

	ctx := context.Background()
	require.NoError(t, srv.messageQueue.Enqueue(ctx, &communication.QueueMessage{
		ID: "m-loop", FromAgent: "dev", ToAgent: "probe", MessageType: "message",
		Payload:  &loomv1.MessagePayload{Data: &loomv1.MessagePayload_Value{Value: []byte("again")}},
		Metadata: map[string]string{communication.MetadataHop: "9"},
	}))
	msg, err := srv.messageQueue.Dequeue(ctx, "probe")
	require.NoError(t, err)
	require.NotNil(t, msg)

	srv.deliverStandaloneMessage(ctx, probe, "probe", msg)

	assert.Equal(t, 0, probeLLM.replyCount(), "no turn runs past the hop limit")
	assert.Equal(t, 0, srv.messageQueue.GetQueueDepth("probe"), "the message left the queue")
	assert.Error(t, srv.messageQueue.Acknowledge(ctx, "m-loop"), "it was rejected, not left in flight")
}

func TestStandaloneRecipient_ExcludesWorkflowMembers(t *testing.T) {
	running := agent.NewAgent(&mockBackend{}, &replyingLLM{})
	alone := agent.NewAgent(&mockBackend{}, &replyingLLM{})
	srv := setupBroadcastTestServer(t, map[string]*agent.Agent{}, nil)
	srv.mu.Lock()
	srv.agents["running"], srv.agents["alone"] = running, alone
	srv.mu.Unlock()

	srv.workflowSubAgentsMu.Lock()
	srv.workflowSubAgents["sess:wf:running"] = &workflowSubAgentContext{agent: running, agentID: "wf:running"}
	srv.workflowSubAgentsMu.Unlock()

	for name, want := range map[string]bool{"running": false, "alone": true, "wf:alone": false, "missing": false} {
		_, _, ok := srv.standaloneRecipient(name)
		assert.Equal(t, want, ok, name)
	}
}
