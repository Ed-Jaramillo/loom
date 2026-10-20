// Copyright 2026 Teradata
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

package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	llmtypes "github.com/teradata-labs/loom/pkg/llm/types"
	"github.com/teradata-labs/loom/pkg/shuttle"
)

// formTool is a fake ParkableQuestion tool.
type formTool struct {
	runs     atomic.Int64
	okToPark bool

	gotInput  atomic.Value // map[string]interface{}
	gotAnswer atomic.Value // string
}

var _ shuttle.ParkableQuestion = (*formTool)(nil)

func (t *formTool) Name() string        { return "form_tool" }
func (t *formTool) Description() string { return "form tool" }
func (t *formTool) InputSchema() *shuttle.JSONSchema {
	return &shuttle.JSONSchema{Type: "object", Properties: map[string]*shuttle.JSONSchema{"title": {Type: "string"}}}
}
func (t *formTool) Backend() string { return "" }
func (t *formTool) Execute(ctx context.Context, params map[string]interface{}) (*shuttle.Result, error) {
	t.runs.Add(1)
	return &shuttle.Result{Success: true, Data: "executed"}, nil
}
func (t *formTool) ParkQuestion(input map[string]interface{}) (string, bool) {
	if !t.okToPark {
		return "", false
	}
	title, _ := input["title"].(string)
	return title, true
}
func (t *formTool) ParkAnswerResult(input map[string]interface{}, answer string) *shuttle.Result {
	t.gotInput.Store(input)
	t.gotAnswer.Store(answer)
	return &shuttle.Result{Success: true, Data: map[string]interface{}{"status": "submitted", "echo": answer}}
}

func formScript(extra ...llmtypes.ToolCall) []mockLLMResponse {
	calls := append([]llmtypes.ToolCall{
		{ID: "c-form", Name: "form_tool", Input: map[string]interface{}{"title": "Name it", "fields": map[string]interface{}{"a": "b"}}},
	}, extra...)
	return []mockLLMResponse{
		{content: "", toolCalls: calls},
		{content: "done"},
	}
}

func newFormFixture(t *testing.T, hooks []shuttle.Hook, ft *formTool, script []mockLLMResponse, tools ...string) *parkFixture {
	t.Helper()
	f := newParkFixture(t, hooks, script, tools...)
	f.ag.RegisterTool(ft)
	return f
}

func toolRow(t *testing.T, f *parkFixture, sessionID, callID string) Message {
	t.Helper()
	sess := f.ag.memory.GetOrCreateSessionWithAgent(context.Background(), sessionID, f.ag.config.Name, "")
	for _, m := range rawSessionMessages(sess) {
		if m.Role == "tool" && m.ToolUseID == callID {
			return m
		}
	}
	t.Fatalf("no tool row for %s", callID)
	return Message{}
}

func TestParkable_ParksAsQuestionWithoutExecuting(t *testing.T) {
	ft := &formTool{okToPark: true}
	f := newFormFixture(t, nil, ft, formScript())

	_, err := f.ag.Chat(context.Background(), "s-pq", "go")
	var parked *TurnParkedError
	if !errors.As(err, &parked) {
		t.Fatalf("expected park, got %v", err)
	}
	hr := f.pendingParked(t, "s-pq")
	if hr.RequestType != "parked" || hr.Kind != "question" {
		t.Fatalf("request shape: %+v", hr)
	}
	desc, ok := hr.Params["c-form"].(map[string]interface{})
	if !ok {
		t.Fatalf("descriptor missing: %#v", hr.Params)
	}
	if desc["kind"] != "question" || desc["tool"] != "form_tool" || desc["question"] != "Name it" {
		t.Fatalf("descriptor = %#v", desc)
	}
	params, _ := desc["params"].(map[string]interface{})
	if params["title"] != "Name it" || params["fields"] == nil {
		t.Fatalf("params = %#v", desc["params"])
	}
	if ft.runs.Load() != 0 {
		t.Fatalf("Execute ran for a parked call")
	}
}

func TestParkable_NotOKRunsExecute(t *testing.T) {
	ft := &formTool{okToPark: false}
	f := newFormFixture(t, nil, ft, formScript())
	resp, err := f.ag.Chat(context.Background(), "s-pq-no", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "done" {
		t.Fatalf("content = %q", resp.Content)
	}
	if ft.runs.Load() != 1 {
		t.Fatalf("Execute ran %d times, want 1", ft.runs.Load())
	}
}

func TestParkable_DenyIsNotParked(t *testing.T) {
	ft := &formTool{okToPark: true}
	f := newFormFixture(t, []shuttle.Hook{scopedDenyHook{tool: "form_tool"}}, ft, formScript())
	resp, err := f.ag.Chat(context.Background(), "s-pq-deny", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "done" {
		t.Fatalf("content = %q", resp.Content)
	}
	if ft.runs.Load() != 0 {
		t.Fatalf("denied tool ran")
	}
	row := toolRow(t, f, "s-pq-deny", "c-form")
	if row.ToolResult == nil || row.ToolResult.Success {
		t.Fatalf("row = %#v, want a denial", row.ToolResult)
	}
}

func parkForm(t *testing.T, f *parkFixture, sid string) *shuttle.HumanRequest {
	t.Helper()
	_, err := f.ag.Chat(context.Background(), sid, "go")
	var parked *TurnParkedError
	if !errors.As(err, &parked) {
		t.Fatalf("expected park, got %v", err)
	}
	return f.pendingParked(t, sid)
}

func TestParkable_ResumeApprovedBuildsResultFromAnswer(t *testing.T) {
	ft := &formTool{okToPark: true}
	f := newFormFixture(t, nil, ft, formScript())
	hr := parkForm(t, f, "s-pq-ok")

	resp, err := f.ag.ResumeChat(context.Background(), "s-pq-ok", ParkDecision{
		RequestID: hr.ID, ItemIDs: paramKeys(hr), Approved: true,
		Answers: map[string]string{"c-form": `{"name":"x"}`},
	}, nil)
	if err != nil {
		t.Fatalf("ResumeChat: %v", err)
	}
	if resp.Content != "done" {
		t.Fatalf("content = %q", resp.Content)
	}
	if got, _ := ft.gotAnswer.Load().(string); got != `{"name":"x"}` {
		t.Fatalf("answer = %q", got)
	}
	in, _ := ft.gotInput.Load().(map[string]interface{})
	if in["title"] != "Name it" {
		t.Fatalf("input = %#v", in)
	}
	row := toolRow(t, f, "s-pq-ok", "c-form")
	data, _ := row.ToolResult.Data.(map[string]interface{})
	if !row.ToolResult.Success || data["status"] != "submitted" || data["echo"] != `{"name":"x"}` {
		t.Fatalf("row = %#v", row.ToolResult)
	}
	if ft.runs.Load() != 0 {
		t.Fatalf("Execute ran on resume")
	}
}

func TestParkable_ResumeRejected(t *testing.T) {
	ft := &formTool{okToPark: true}
	f := newFormFixture(t, nil, ft, formScript())
	hr := parkForm(t, f, "s-pq-rej")

	if _, err := f.ag.ResumeChat(context.Background(), "s-pq-rej", ParkDecision{
		RequestID: hr.ID, ItemIDs: paramKeys(hr), Approved: false, Reason: "rejected by user: nope",
	}, nil); err != nil {
		t.Fatalf("ResumeChat: %v", err)
	}
	row := toolRow(t, f, "s-pq-rej", "c-form")
	if row.ToolResult.Success || row.ToolResult.Error == nil ||
		row.ToolResult.Error.Code != "permission_denied" || row.ToolResult.Error.Message != "rejected by user: nope" {
		t.Fatalf("row = %#v", row.ToolResult)
	}
}

func TestParkable_ResumeEmptyAnswerIsMissing(t *testing.T) {
	ft := &formTool{okToPark: true}
	f := newFormFixture(t, nil, ft, formScript())
	hr := parkForm(t, f, "s-pq-empty")

	if _, err := f.ag.ResumeChat(context.Background(), "s-pq-empty", ParkDecision{
		RequestID: hr.ID, ItemIDs: paramKeys(hr), Approved: true, Answers: map[string]string{"c-form": ""},
	}, nil); err != nil {
		t.Fatalf("ResumeChat: %v", err)
	}
	row := toolRow(t, f, "s-pq-empty", "c-form")
	if row.ToolResult.Success || row.ToolResult.Error == nil || row.ToolResult.Error.Code != "MISSING_ANSWER" {
		t.Fatalf("row = %#v", row.ToolResult)
	}
}

func TestParkable_MixedBatchWithApprovalItem(t *testing.T) {
	ft := &formTool{okToPark: true}
	script := formScript(llmtypes.ToolCall{ID: "c-write", Name: "export_csv", Input: map[string]interface{}{"v": "w"}})
	f := newFormFixture(t, []shuttle.Hook{scopedAskHook{tool: "export_csv"}}, ft, script, "export_csv")

	hr := parkForm(t, f, "s-pq-mixed")
	if hr.Kind != "approval" {
		t.Fatalf("kind = %q, want approval for a mixed batch", hr.Kind)
	}
	if len(hr.Params) != 2 {
		t.Fatalf("params keys = %v", paramKeys(hr))
	}
	if _, err := f.ag.ResumeChat(context.Background(), "s-pq-mixed", ParkDecision{
		RequestID: hr.ID, ItemIDs: paramKeys(hr), Approved: true,
		Answers: map[string]string{"c-form": `{"a":1}`},
	}, nil); err != nil {
		t.Fatalf("ResumeChat: %v", err)
	}
	if got := f.tools["export_csv"].runs.Load(); got != 1 {
		t.Fatalf("export_csv ran %d times, want 1", got)
	}
	if got, _ := ft.gotAnswer.Load().(string); got != `{"a":1}` {
		t.Fatalf("answer = %q", got)
	}
	if ft.runs.Load() != 0 {
		t.Fatalf("form Execute ran")
	}
}
