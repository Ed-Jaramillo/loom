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

package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/teradata-labs/loom/pkg/observability/phoenix"
)

const (
	testTrace   = "d8f19f723c0b840e788cbfa0aed7c22c"
	testProject = "default"
	testNodeID  = "UHJvamVjdDox"
)

var testModel = phoenix.ModelSelection{ProviderType: "builtin", Provider: "ANTHROPIC", ModelName: "claude-test"}

// fakePhoenix is a scripted stand-in for the Phoenix 20.22.0 agent-session
// routes. Each chat call pops the next scripted turn; the transcript is kept
// in sync with what the stream announced so the tool's transcript reads see
// the same state a real Phoenix would persist.
type fakePhoenix struct {
	t *testing.T

	mu            sync.Mutex
	chatBodies    []map[string]any
	turns         []scriptedTurn // popped per chat call
	transcript    []json.RawMessage
	sessions      int
	spansForTrace int
	busyOnce      bool
	failMessages  bool
}

type scriptedTurn struct {
	// stream is the SSE body returned for the chat call.
	stream string
	// assistant is the transcript message appended after the stream.
	assistant map[string]any
}

func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

func (f *fakePhoenix) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer k" {
				http.Error(w, "Invalid token", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/projects/{p}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("p") != testProject {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"id":%q,"name":%q}}`, testNodeID, testProject)
	}))
	mux.HandleFunc("GET /v1/projects/{p}/spans", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := f.spansForTrace
		f.mu.Unlock()
		if r.URL.Query().Get("trace_id") != testTrace {
			n = 0
		}
		var spans []string
		for i := 0; i < n; i++ {
			spans = append(spans, fmt.Sprintf(`{"name":"llm.completion","context":{"trace_id":%q,"span_id":"%016x"},"span_kind":"LLM","start_time":"2026-10-20T00:00:00Z","end_time":"2026-10-20T00:00:01Z","status_code":"OK","attributes":{}}`, testTrace, i+1))
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s],"next_cursor":null}`, strings.Join(spans, ","))
	}))
	mux.HandleFunc("POST /v1/agent_sessions", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		require.NoError(f.t, json.Unmarshal(raw, &body))
		assert.Equal(f.t, true, body["is_ephemeral"])
		f.mu.Lock()
		f.sessions++
		id := fmt.Sprintf("session-%d", f.sessions)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"data":{"id":%q}}`, id)
	}))
	mux.HandleFunc("GET /v1/agent_sessions/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"data":{"id":%q,"title":"","created_at":"2026-10-20T00:00:00Z","updated_at":"2026-10-20T00:00:00Z","is_ephemeral":true,"model":{"providerType":"builtin","provider":"ANTHROPIC","modelName":"claude-test"},"is_active":false}}`, r.PathValue("id"))
	}))
	mux.HandleFunc("GET /v1/agent_sessions/{id}/messages", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failMessages {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		parts := make([]string, 0, len(f.transcript))
		for _, m := range f.transcript {
			parts = append(parts, string(m))
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s],"next_cursor":null}`, strings.Join(parts, ","))
	}))
	mux.HandleFunc("POST /v1/agent_sessions/{id}/chat", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		require.NoError(f.t, json.Unmarshal(raw, &body))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.chatBodies = append(f.chatBodies, body)
		if f.busyOnce {
			f.busyOnce = false
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"agent_session_busy"}`))
			return
		}
		// Persist the user message, if any, the way Phoenix does.
		if msg, ok := body["message"].(map[string]any); ok {
			raw, _ := json.Marshal(msg)
			f.transcript = append(f.transcript, raw)
		}
		// Approvals must be answered against the trailing assistant message.
		if approvals, ok := body["toolApprovals"].([]any); ok {
			last := f.transcript[len(f.transcript)-1]
			var lastMsg map[string]any
			_ = json.Unmarshal(last, &lastMsg)
			assert.Equal(f.t, lastMsg["id"], body["lastMessageId"], "lastMessageId must be the trailing assistant message")
			for _, a := range approvals {
				am := a.(map[string]any)
				for _, p := range lastMsg["parts"].([]any) {
					pm := p.(map[string]any)
					if pm["toolCallId"] == am["toolCallId"] {
						pm["state"] = "approval-responded"
						pm["approval"] = map[string]any{"id": pm["approval"].(map[string]any)["id"], "approved": am["approved"]}
					}
				}
			}
			raw, _ := json.Marshal(lastMsg)
			f.transcript[len(f.transcript)-1] = raw
		}
		if len(f.turns) == 0 {
			http.Error(w, "no scripted turn", http.StatusInternalServerError)
			return
		}
		turn := f.turns[0]
		f.turns = f.turns[1:]
		if turn.assistant != nil {
			raw, _ := json.Marshal(turn.assistant)
			f.transcript = append(f.transcript, raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(turn.stream))
	}))
	return mux
}

func newFake(t *testing.T) (*fakePhoenix, *phoenix.Client) {
	t.Helper()
	f := &fakePhoenix{t: t, spansForTrace: 3}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := phoenix.New(phoenix.Config{BaseURL: srv.URL, APIKey: "k", RetryBackoff: time.Millisecond})
	require.NoError(t, err)
	return f, c
}

func assistantMsg(id string, parts ...map[string]any) map[string]any {
	anyParts := make([]any, 0, len(parts))
	for _, p := range parts {
		anyParts = append(anyParts, p)
	}
	return map[string]any{
		"id":   id,
		"role": "assistant",
		"metadata": map[string]any{"phoenix": map[string]any{
			"type": "assistant", "sessionId": "session-1",
			"turnTraceContext": map[string]any{"traceId": strings.Repeat("a", 32), "rootSpanId": strings.Repeat("b", 16), "startedAt": "2026-10-20T00:00:00Z"},
			"usage":            map[string]any{"tokens": map[string]any{"prompt": 100, "completion": 10, "total": 110}},
		}},
		"parts": anyParts,
	}
}

func TestNew_Validation(t *testing.T) {
	_, c := newFake(t)
	_, err := New(nil, Config{Project: "p", Model: testModel})
	assert.Error(t, err)
	_, err = New(c, Config{Project: "p"})
	assert.Error(t, err, "model is required")
	_, err = New(c, Config{Model: testModel})
	assert.Error(t, err, "project is required")
	tool, err := New(c, Config{Project: "p", Model: testModel})
	require.NoError(t, err)
	assert.Equal(t, ToolName, tool.Name())
	assert.Equal(t, "", tool.Backend())
	assert.ElementsMatch(t, []string{"question", "trace_id"}, tool.InputSchema().Required)
	assert.Equal(t, DefaultTimeout, tool.cfg.Timeout)
}

func TestExecute_ParamValidation(t *testing.T) {
	f, c := newFake(t)
	tool, err := New(c, Config{Project: testProject, Model: testModel})
	require.NoError(t, err)

	cases := []map[string]interface{}{
		{"trace_id": testTrace},
		{"question": "q"},
		{"question": "q", "trace_id": "short"},
		{"question": "q", "trace_id": testTrace, "span_id": "zz"},
	}
	for _, params := range cases {
		res, err := tool.Execute(context.Background(), params)
		require.NoError(t, err)
		assert.False(t, res.Success)
		assert.Equal(t, "INVALID_PARAMS", res.Error.Code)
	}
	assert.Equal(t, 0, f.sessions, "no session is created for invalid input")
}

func TestExecute_TraceNotFoundCreatesNoSession(t *testing.T) {
	f, c := newFake(t)
	f.spansForTrace = 0
	tool, err := New(c, Config{Project: testProject, Model: testModel})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"question": "q", "trace_id": testTrace})
	require.NoError(t, err)
	assert.False(t, res.Success)
	assert.Equal(t, "TRACE_NOT_FOUND", res.Error.Code)
	assert.Equal(t, 0, f.sessions)
}

func TestExecute_FullFlowWithApprovals(t *testing.T) {
	f, c := newFake(t)
	f.turns = []scriptedTurn{
		{
			stream: sse(
				`{"type":"start","messageId":"assistant-1"}`,
				`{"type":"text-start","id":"t1"}`,
				`{"type":"text-delta","id":"t1","delta":"Looking at the trace."}`,
				`{"type":"text-end","id":"t1"}`,
				`{"type":"tool-input-available","toolCallId":"c-read","toolName":"get_spans","input":{"trace_id":"`+testTrace+`"}}`,
				`{"type":"tool-output-available","toolCallId":"c-read","output":[]}`,
				`{"type":"tool-input-available","toolCallId":"c-sql","toolName":"execute_sql","input":{"sql":"select 1"}}`,
				`{"type":"tool-approval-request","approvalId":"ap-1","toolCallId":"c-sql"}`,
				`{"type":"tool-input-available","toolCallId":"c-write","toolName":"save_prompt","input":{"name":"p"}}`,
				`{"type":"tool-approval-request","approvalId":"ap-2","toolCallId":"c-write"}`,
				`{"type":"brand-new-chunk","x":1}`,
				`{"type":"finish","finishReason":"tool-calls"}`,
				`[DONE]`,
			),
			assistant: assistantMsg("assistant-1",
				map[string]any{"type": "text", "text": "Looking at the trace.", "state": "done"},
				map[string]any{"type": "tool-get_spans", "toolCallId": "c-read", "state": "output-available", "input": map[string]any{"trace_id": testTrace}, "output": []any{}},
				map[string]any{"type": "tool-execute_sql", "toolCallId": "c-sql", "state": "approval-requested", "input": map[string]any{"sql": "select 1"}, "approval": map[string]any{"id": "ap-1"}},
				map[string]any{"type": "tool-save_prompt", "toolCallId": "c-write", "state": "approval-requested", "input": map[string]any{"name": "p"}, "approval": map[string]any{"id": "ap-2"}},
			),
		},
		{
			stream: sse(
				`{"type":"start","messageId":"assistant-2"}`,
				`{"type":"tool-output-available","toolCallId":"c-sql","output":[{"n":1}]}`,
				`{"type":"tool-output-denied","toolCallId":"c-write"}`,
				`{"type":"text-delta","id":"t2","delta":"The call was truncated at max_tokens (span 0000000000000001)."}`,
				`{"type":"finish","finishReason":"stop"}`,
				`[DONE]`,
			),
			assistant: assistantMsg("assistant-2",
				map[string]any{"type": "tool-execute_sql", "toolCallId": "c-sql", "state": "output-available", "input": map[string]any{"sql": "select 1"}, "output": []any{map[string]any{"n": 1}}},
				map[string]any{"type": "tool-save_prompt", "toolCallId": "c-write", "state": "output-denied", "input": map[string]any{"name": "p"}},
				map[string]any{"type": "text", "text": "The call was truncated at max_tokens (span 0000000000000001).", "state": "done"},
			),
		},
	}
	tool, err := New(c, Config{
		Project:      testProject,
		Model:        testModel,
		ApproveTools: []string{"execute_sql"},
		PayloadBytes: 64,
	})
	require.NoError(t, err)
	tool.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }

	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"question": "Why did this run trip the breaker?",
		"trace_id": strings.ToUpper(testTrace),
		"span_id":  "0000000000000001",
	})
	require.NoError(t, err)
	require.True(t, res.Success, "%+v", res.Error)
	ans, ok := res.Data.(*Answer)
	require.True(t, ok)

	// Request shape of the first turn.
	require.Len(t, f.chatBodies, 2)
	first := f.chatBodies[0]
	assert.Equal(t, true, first["headless"])
	assert.Equal(t, "manual", first["editPermission"])
	ctxs := first["contexts"].([]any)
	types := make([]string, 0, len(ctxs))
	for _, cx := range ctxs {
		types = append(types, cx.(map[string]any)["type"].(string))
	}
	assert.Equal(t, []string{"app", "trace", "span", "graphql", "web_access", "subagents"}, types)
	assert.Equal(t, map[string]any{"type": "trace", "projectNodeId": testNodeID, "otelTraceId": testTrace}, ctxs[1])
	assert.Equal(t, "Why did this run trip the breaker?", first["message"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"])

	// Approval continuation: allowlisted tool approved, the write denied.
	second := f.chatBodies[1]
	_, hasMessage := second["message"]
	assert.False(t, hasMessage)
	assert.Equal(t, "assistant-1", second["lastMessageId"])
	approvals := second["toolApprovals"].([]any)
	decisions := map[string]bool{}
	for _, a := range approvals {
		am := a.(map[string]any)
		decisions[am["toolCallId"].(string)] = am["approved"].(bool)
	}
	assert.Equal(t, map[string]bool{"c-sql": true, "c-write": false}, decisions)

	// Answer assembled from the transcript.
	assert.Equal(t, "Looking at the trace.\nThe call was truncated at max_tokens (span 0000000000000001).", ans.Answer)
	assert.Equal(t, untrustedNote, ans.Note)
	assert.Equal(t, testTrace, ans.TraceID)
	assert.Equal(t, "0000000000000001", ans.SpanID)
	assert.Equal(t, testNodeID, ans.ProjectNodeID)
	assert.Equal(t, "session-1", ans.SessionID)
	assert.Equal(t, 1, ans.ApprovalRounds)
	assert.False(t, ans.ApprovalsUnresolved)
	assert.Equal(t, []string{"execute_sql"}, ans.ApprovedTools)
	assert.Equal(t, []string{"save_prompt"}, ans.DeniedTools)
	assert.True(t, ans.Usage.Reported)
	assert.Equal(t, int64(220), ans.Usage.TotalTokens, "usage summed over both assistant messages")
	assert.Equal(t, []string{strings.Repeat("a", 32)}, ans.AssistantTraceIDs)
	assert.Contains(t, ans.StreamChunkTypes, "tool-approval-request")
	assert.Contains(t, ans.StreamChunkTypes, "brand-new-chunk")
	require.Len(t, ans.UnknownChunkSamples, 1)
	assert.Equal(t, true, res.Metadata["untrusted"])

	byID := map[string]ToolCallSummary{}
	for _, tc := range ans.ToolCalls {
		byID[tc.ToolCallID+"/"+tc.State] = tc
	}
	assert.Equal(t, "approved", byID["c-sql/output-available"].ApprovalDecision)
	assert.Equal(t, "denied", byID["c-write/output-denied"].ApprovalDecision)
	assert.Equal(t, "", byID["c-read/output-available"].ApprovalDecision)
	assert.Equal(t, "get_spans", byID["c-read/output-available"].Name)

	// The result must marshal (it is handed to the model as JSON).
	_, err = json.Marshal(res.Data)
	require.NoError(t, err)
}

func TestExecute_MaxApprovalRoundsLeavesUnresolved(t *testing.T) {
	f, c := newFake(t)
	pendingMsg := func(id string) map[string]any {
		return assistantMsg(id,
			map[string]any{"type": "tool-save_prompt", "toolCallId": "w-" + id, "state": "approval-requested", "input": map[string]any{}, "approval": map[string]any{"id": "ap-" + id}},
		)
	}
	f.turns = []scriptedTurn{
		{stream: sse(`{"type":"start","messageId":"a1"}`, `{"type":"finish"}`), assistant: pendingMsg("a1")},
		{stream: sse(`{"type":"start","messageId":"a2"}`, `{"type":"finish"}`), assistant: pendingMsg("a2")},
	}
	tool, err := New(c, Config{Project: testProject, Model: testModel, MaxApprovalRounds: 1})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"question": "q", "trace_id": testTrace})
	require.NoError(t, err)
	require.True(t, res.Success)
	ans := res.Data.(*Answer)
	assert.Equal(t, 1, ans.ApprovalRounds)
	assert.True(t, ans.ApprovalsUnresolved)
	assert.Equal(t, []string{"save_prompt"}, ans.DeniedTools)
	assert.Len(t, f.chatBodies, 2)
}

func TestExecute_BusySessionRetriesOnce(t *testing.T) {
	f, c := newFake(t)
	f.busyOnce = true
	f.turns = []scriptedTurn{{
		stream:    sse(`{"type":"start","messageId":"a1"}`, `{"type":"text-delta","id":"t","delta":"ok"}`, `{"type":"finish"}`),
		assistant: assistantMsg("a1", map[string]any{"type": "text", "text": "ok"}),
	}}
	tool, err := New(c, Config{Project: testProject, Model: testModel})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"question": "q", "trace_id": testTrace})
	require.NoError(t, err)
	require.True(t, res.Success, "%+v", res.Error)
	assert.Equal(t, "ok", res.Data.(*Answer).Answer)
	assert.Len(t, f.chatBodies, 2, "the busy 409 and the retry")
}

func TestExecute_TranscriptUnavailableFallsBackToStream(t *testing.T) {
	f, c := newFake(t)
	f.failMessages = true
	f.turns = []scriptedTurn{{
		stream: sse(`{"type":"start","messageId":"a1"}`, `{"type":"text-delta","id":"t","delta":"from stream"}`, `{"type":"finish"}`),
	}}
	tool, err := New(c, Config{Project: testProject, Model: testModel})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"question": "q", "trace_id": testTrace})
	require.NoError(t, err)
	require.True(t, res.Success)
	ans := res.Data.(*Answer)
	assert.Equal(t, "from stream", ans.Answer)
	assert.False(t, ans.Usage.Reported)
	require.NotEmpty(t, ans.Warnings)
	assert.Contains(t, strings.Join(ans.Warnings, "|"), "transcript unavailable")
}

func TestExecute_IncompleteStreamIsWarningNotFailure(t *testing.T) {
	f, c := newFake(t)
	f.turns = []scriptedTurn{{
		stream:    sse(`{"type":"start","messageId":"a1"}`, `{"type":"text-delta","id":"t","delta":"half"}`),
		assistant: assistantMsg("a1", map[string]any{"type": "text", "text": "half"}),
	}}
	tool, err := New(c, Config{Project: testProject, Model: testModel})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"question": "q", "trace_id": testTrace})
	require.NoError(t, err)
	require.True(t, res.Success)
	ans := res.Data.(*Answer)
	assert.Equal(t, "half", ans.Answer)
	assert.Contains(t, strings.Join(ans.Warnings, "|"), "without a terminal chunk")
}

func TestExecute_UnauthorizedIsReported(t *testing.T) {
	f, _ := newFake(t)
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := phoenix.New(phoenix.Config{BaseURL: srv.URL, APIKey: "wrong", RetryBackoff: time.Millisecond})
	require.NoError(t, err)
	tool, err := New(c, Config{Project: testProject, Model: testModel})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"question": "q", "trace_id": testTrace})
	require.NoError(t, err)
	assert.False(t, res.Success)
	assert.Equal(t, "PHOENIX_PROJECT", res.Error.Code)
	assert.NotContains(t, res.Error.Message, "wrong", "the key never appears in errors")
}
