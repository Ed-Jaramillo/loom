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

package phoenix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testModel = ModelSelection{ProviderType: "builtin", Provider: "ANTHROPIC", ModelName: "claude-test"}

func TestModelSelection_Validate(t *testing.T) {
	assert.NoError(t, testModel.Validate())
	assert.NoError(t, ModelSelection{ProviderType: "custom", ProviderID: "p", ModelName: "m"}.Validate())
	assert.Error(t, ModelSelection{ProviderType: "builtin", ModelName: "m"}.Validate())
	assert.Error(t, ModelSelection{ProviderType: "custom", ModelName: "m"}.Validate())
	assert.Error(t, ModelSelection{ProviderType: "other", Provider: "X", ModelName: "m"}.Validate())
}

func TestCreateAgentSession(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/agent_sessions", r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"QWdlbnRTZXNzaW9uOjE="}}`))
	}), nil)

	id, err := c.CreateAgentSession(context.Background(), CreateSessionRequest{
		Model: testModel, Title: strings.Repeat("t", 150), IsEphemeral: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "QWdlbnRTZXNzaW9uOjE=", id)
	assert.Equal(t, true, body["is_ephemeral"])
	assert.Len(t, body["title"], 100, "title is clipped to the spec's maxLength")
	model := body["model"].(map[string]any)
	assert.Equal(t, "builtin", model["providerType"])
	assert.Equal(t, "ANTHROPIC", model["provider"])
	_, hasProviderID := model["providerId"]
	assert.False(t, hasProviderID, "custom-only field omitted for builtin")
}

func TestChat_SendsSpecShapedRequestAndParsesStream(t *testing.T) {
	var got map[string]any
	var accept string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/agent_sessions/QWdlbnRTZXNzaW9uOjE=/chat", r.URL.Path)
		accept = r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &got))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(
			`{"type":"start","messageId":"assistant-1"}`,
			`{"type":"text-delta","id":"t","delta":"Hello"}`,
			`{"type":"finish","finishReason":"stop"}`,
			`[DONE]`,
		)))
	}), nil)

	tc, err := NewTraceContext("UHJvamVjdDox", "d8f19f723c0b840e788cbfa0aed7c22c")
	require.NoError(t, err)
	turn, err := c.Chat(context.Background(), "QWdlbnRTZXNzaW9uOjE=", ChatRequest{
		Headless: true,
		Contexts: append([]any{NewAppContext(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)), tc}, DefaultToggleContexts()...),
		Model:    testModel,
		Message:  NewUserMessage("What failed?"),
	}, StreamParserConfig{})
	require.NoError(t, err)
	assert.Equal(t, "Hello", turn.Text)
	assert.Equal(t, "assistant-1", turn.MessageID)
	assert.Contains(t, accept, "text/event-stream")

	assert.Equal(t, true, got["headless"])
	assert.Equal(t, "manual", got["editPermission"])
	assert.Equal(t, "submit-message", got["trigger"])
	// Phoenix rejects any message id that is not a bare UUID when it saves
	// the turn, which empties the reply.
	_, err = uuid.Parse(got["id"].(string))
	assert.NoError(t, err, "chat request id is a bare UUID")
	_, hasLast := got["lastMessageId"]
	assert.False(t, hasLast, "lastMessageId omitted on an empty transcript")
	_, hasApprovals := got["toolApprovals"]
	assert.False(t, hasApprovals)
	msg := got["message"].(map[string]any)
	assert.Equal(t, "user", msg["role"])
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, msg["id"],
		"user message id matches Phoenix's valid_message_id constraint")
	parts := msg["parts"].([]any)
	assert.Equal(t, map[string]any{"type": "text", "text": "What failed?"}, parts[0])
	ctxs := got["contexts"].([]any)
	require.Len(t, ctxs, 5)
	assert.Equal(t, "app", ctxs[0].(map[string]any)["type"])
	assert.Equal(t, map[string]any{"type": "trace", "projectNodeId": "UHJvamVjdDox", "otelTraceId": "d8f19f723c0b840e788cbfa0aed7c22c"}, ctxs[1])
	assert.Equal(t, map[string]any{"type": "graphql", "mutationsEnabled": false}, ctxs[2])
	assert.Equal(t, map[string]any{"type": "web_access", "enabled": false}, ctxs[3])
	assert.Equal(t, map[string]any{"type": "subagents", "enabled": false}, ctxs[4])
}

func TestChat_ApprovalContinuationAndValidation(t *testing.T) {
	var got map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &got))
		_, _ = w.Write([]byte(sse(`{"type":"finish"}`, `[DONE]`)))
	}), nil)
	last := "assistant-1"
	_, err := c.Chat(context.Background(), "s", ChatRequest{
		Headless:      true,
		Model:         testModel,
		ToolApprovals: []ToolApproval{{ToolCallID: "c1", Approved: false}},
		LastMessageID: &last,
	}, StreamParserConfig{})
	require.NoError(t, err)
	assert.Equal(t, "assistant-1", got["lastMessageId"])
	_, hasMessage := got["message"]
	assert.False(t, hasMessage)
	approvals := got["toolApprovals"].([]any)
	assert.Equal(t, map[string]any{"toolCallId": "c1", "approved": false}, approvals[0])

	_, err = c.Chat(context.Background(), "s", ChatRequest{Model: testModel}, StreamParserConfig{})
	require.Error(t, err, "needs a message or approvals")
	_, err = c.Chat(context.Background(), "s", ChatRequest{
		Model: testModel, Message: NewUserMessage("x"), ToolApprovals: []ToolApproval{{ToolCallID: "c"}},
	}, StreamParserConfig{})
	require.Error(t, err, "message and approvals cannot be combined")
}

func TestChat_ConflictAndIncompleteStream(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"agent_session_model_stale"}`))
			return
		}
		_, _ = w.Write([]byte(sse(`{"type":"start","messageId":"a"}`, `{"type":"text-delta","id":"t","delta":"half"}`)))
	}), nil)

	_, err := c.Chat(context.Background(), "s", ChatRequest{Model: testModel, Message: NewUserMessage("q")}, StreamParserConfig{})
	assert.True(t, IsConflict(err, ConflictModelStale))

	turn, err := c.Chat(context.Background(), "s", ChatRequest{Model: testModel, Message: NewUserMessage("q")}, StreamParserConfig{})
	assert.True(t, errors.Is(err, ErrStreamIncomplete))
	require.NotNil(t, turn)
	assert.Equal(t, "half", turn.Text)
}

func TestChat_ContextDeadlineBoundsStream(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}), func(c *Config) { c.RequestTimeout = 10 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Chat(ctx, "s", ChatRequest{Model: testModel, Message: NewUserMessage("q")}, StreamParserConfig{})
	require.Error(t, err)
	// The per-request timeout does not apply to the stream; the caller's deadline does.
	assert.Greater(t, time.Since(start), 50*time.Millisecond)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestListAgentSessionMessages_DecodesPartsAndMetadata(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"user-1","role":"user","parts":[{"type":"text","text":"What failed?"}]}],"next_cursor":"c2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"assistant-1","role":"assistant","metadata":{"phoenix":{"type":"assistant","sessionId":"s","turnTraceContext":{"traceId":"0123456789abcdef0123456789abcdef","rootSpanId":"0123456789abcdef","startedAt":"2026-10-20T00:00:00Z"},"usage":{"tokens":{"prompt":100,"completion":20,"total":120},"promptDetails":{"cacheRead":50,"cacheWrite":0}}}},"parts":[{"type":"step-start"},{"type":"tool-save_prompt","toolCallId":"c1","state":"approval-requested","input":{"a":1},"approval":{"id":"ap-1"}},{"type":"dynamic-tool","toolName":"get_spans","toolCallId":"c2","state":"output-available","input":{},"output":[]},{"type":"text","text":"Done.","state":"done"},{"not":"a part"}]}],"next_cursor":null}`))
	}), nil)

	msgs, truncated, err := c.ListAgentSessionMessages(context.Background(), "s")
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, msgs, 2)
	assert.Nil(t, msgs[0].AssistantMetadata())

	a := msgs[1]
	parts, skipped := a.DecodedParts()
	assert.Equal(t, 1, skipped)
	require.Len(t, parts, 4)
	assert.True(t, parts[1].IsTool())
	assert.Equal(t, "save_prompt", parts[1].Name())
	assert.Equal(t, ToolStateApprovalRequested, parts[1].State)
	assert.Equal(t, "ap-1", parts[1].Approval.ID)
	assert.Equal(t, "get_spans", parts[2].Name())
	assert.False(t, parts[3].IsTool())
	assert.Equal(t, "Done.", parts[3].Text)

	md := a.AssistantMetadata()
	require.NotNil(t, md)
	assert.Equal(t, int64(120), md.Usage.Tokens.Total)
	assert.Equal(t, int64(50), md.Usage.PromptDetails.CacheRead)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", md.TurnTraceContext.TraceID)
}

func TestWaitUntilIdle(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active := atomic.AddInt32(&calls, 1) < 3
		_, _ = w.Write([]byte(`{"data":{"id":"s","title":"","created_at":"2026-10-20T00:00:00Z","updated_at":"2026-10-20T00:00:00Z","is_ephemeral":true,"model":{"providerType":"builtin","provider":"ANTHROPIC","modelName":"m"},"is_active":` + map[bool]string{true: "true", false: "false"}[active] + `}}`))
	}), nil)
	require.NoError(t, c.WaitUntilIdle(context.Background(), "s", time.Millisecond))
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	atomic.StoreInt32(&calls, -1000)
	assert.Error(t, c.WaitUntilIdle(ctx, "s", time.Millisecond))
}

func TestTraceContextValidation(t *testing.T) {
	_, err := NewTraceContext("", "d8f19f723c0b840e788cbfa0aed7c22c")
	assert.Error(t, err)
	_, err = NewTraceContext("p", "D8F19F723C0B840E788CBFA0AED7C22C")
	assert.Error(t, err, "uppercase hex is rejected by Phoenix's pattern")
	assert.True(t, ValidSpanID("0123456789abcdef"))
	assert.False(t, ValidSpanID("0123"))
}

// Ensure httptest import is used even if the helper moves.
var _ = httptest.NewServer
