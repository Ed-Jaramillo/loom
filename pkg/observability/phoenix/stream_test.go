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
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sse joins JSON chunks into an SSE body the way Phoenix emits them.
func sse(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: ")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	return b.String()
}

func TestParseChatStream_TextAndTools(t *testing.T) {
	body := sse(
		`{"type":"start","messageId":"assistant-1"}`,
		`{"type":"start-step"}`,
		`{"type":"reasoning-start","id":"r1"}`,
		`{"type":"reasoning-delta","id":"r1","delta":"thinking"}`,
		`{"type":"reasoning-end","id":"r1"}`,
		`{"type":"tool-input-start","toolCallId":"call-1","toolName":"get_spans"}`,
		`{"type":"tool-input-delta","toolCallId":"call-1","inputTextDelta":"{\"tra"}`,
		`{"type":"tool-input-available","toolCallId":"call-1","toolName":"get_spans","input":{"trace_id":"abc"}}`,
		`{"type":"tool-output-available","toolCallId":"call-1","output":[{"span":"s1"}]}`,
		`{"type":"text-start","id":"t1"}`,
		`{"type":"text-delta","id":"t1","delta":"The run "}`,
		`{"type":"text-delta","id":"t1","delta":"hit max_tokens."}`,
		`{"type":"text-end","id":"t1"}`,
		`{"type":"data-session-summary","data":"Investigate"}`,
		`{"type":"finish-step"}`,
		`{"type":"finish","finishReason":"stop","messageMetadata":{"phoenix":{"type":"assistant","sessionId":"s"}}}`,
		`[DONE]`,
	)
	turn, err := ParseChatStream(strings.NewReader(body), StreamParserConfig{})
	require.NoError(t, err)
	assert.True(t, turn.Complete)
	assert.Equal(t, "assistant-1", turn.MessageID)
	assert.Equal(t, "The run hit max_tokens.", turn.Text)
	assert.Equal(t, len("thinking"), turn.ReasoningChars)
	assert.Equal(t, "stop", turn.FinishReason)
	assert.JSONEq(t, `{"phoenix":{"type":"assistant","sessionId":"s"}}`, string(turn.MessageMetadata))
	require.Len(t, turn.ToolCalls, 1)
	tc := turn.ToolCalls[0]
	assert.Equal(t, "call-1", tc.ToolCallID)
	assert.Equal(t, "get_spans", tc.ToolName)
	assert.Equal(t, ToolStateOutputAvailable, tc.State)
	assert.JSONEq(t, `{"trace_id":"abc"}`, string(tc.Input))
	assert.JSONEq(t, `[{"span":"s1"}]`, string(tc.Output))
	assert.Equal(t, 2, turn.ChunkCounts[ChunkTextDelta])
	assert.Empty(t, turn.UnknownChunkSamples)
	assert.Empty(t, turn.PendingApprovals())
}

func TestParseChatStream_ApprovalAndDenial(t *testing.T) {
	body := sse(
		`{"type":"start","messageId":"assistant-2"}`,
		`{"type":"tool-input-available","toolCallId":"c1","toolName":"save_prompt","input":{"x":1}}`,
		`{"type":"tool-approval-request","approvalId":"ap-1","toolCallId":"c1"}`,
		`{"type":"tool-input-available","toolCallId":"c2","toolName":"delete_project","input":{}}`,
		`{"type":"tool-output-denied","toolCallId":"c2"}`,
		`{"type":"tool-input-available","toolCallId":"c3","toolName":"bash","input":{}}`,
		`{"type":"tool-output-error","toolCallId":"c3","errorText":"boom"}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
		`[DONE]`,
	)
	turn, err := ParseChatStream(strings.NewReader(body), StreamParserConfig{})
	require.NoError(t, err)
	require.Len(t, turn.ToolCalls, 3)
	pending := turn.PendingApprovals()
	require.Len(t, pending, 1)
	assert.Equal(t, "c1", pending[0].ToolCallID)
	assert.Equal(t, "ap-1", pending[0].ApprovalID)
	assert.Equal(t, "save_prompt", pending[0].ToolName)
	assert.Equal(t, ToolStateOutputDenied, turn.ToolCalls[1].State)
	assert.Equal(t, ToolStateOutputError, turn.ToolCalls[2].State)
	assert.Equal(t, "boom", turn.ToolCalls[2].ErrorText)
}

func TestParseChatStream_ErrorAndAbort(t *testing.T) {
	body := sse(
		`{"type":"start","messageId":"a"}`,
		`{"type":"error","errorText":"provider failed"}`,
		`{"type":"abort","reason":"interrupted"}`,
	)
	turn, err := ParseChatStream(strings.NewReader(body), StreamParserConfig{})
	require.NoError(t, err)
	assert.True(t, turn.Complete)
	assert.True(t, turn.Aborted)
	assert.Equal(t, "interrupted", turn.AbortReason)
	assert.Equal(t, []string{"provider failed"}, turn.Errors)
}

func TestParseChatStream_IncompleteStream(t *testing.T) {
	body := sse(
		`{"type":"start","messageId":"a"}`,
		`{"type":"text-delta","id":"t","delta":"partial"}`,
	)
	turn, err := ParseChatStream(strings.NewReader(body), StreamParserConfig{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrStreamIncomplete))
	require.NotNil(t, turn)
	assert.Equal(t, "partial", turn.Text)
	assert.False(t, turn.Complete)
}

func TestParseChatStream_UnknownChunksAndGarbage(t *testing.T) {
	body := sse(
		`{"type":"start"}`,
		`{"type":"brand-new-chunk","foo":1}`,
		`not json at all`,
		`{"type":"finish"}`,
		`[DONE]`,
	) + ": sse comment\nevent: message\nid: 7\n"
	turn, err := ParseChatStream(strings.NewReader(body), StreamParserConfig{MaxUnknownSamples: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, turn.ChunkCounts["brand-new-chunk"])
	assert.Equal(t, 1, turn.ChunkCounts["<unparseable>"])
	// Cap of one sample: the first unknown wins.
	require.Len(t, turn.UnknownChunkSamples, 1)
	assert.Contains(t, turn.UnknownChunkSamples[0], "brand-new-chunk")
	assert.ElementsMatch(t, []string{"<unparseable>", "brand-new-chunk", "finish", "start"}, turn.ChunkTypes())
}

func TestParseChatStream_PayloadCap(t *testing.T) {
	big := strings.Repeat("x", 100)
	body := sse(
		`{"type":"tool-input-available","toolCallId":"c","toolName":"t","input":{"blob":"`+big+`"}}`,
		`{"type":"finish"}`,
	)
	turn, err := ParseChatStream(strings.NewReader(body), StreamParserConfig{MaxPayloadBytes: 32})
	require.NoError(t, err)
	require.Len(t, turn.ToolCalls, 1)
	assert.True(t, turn.ToolCalls[0].InputTruncated)
	// The truncated payload is stored as a JSON string so the turn still marshals.
	var s string
	require.NoError(t, jsonUnmarshal(turn.ToolCalls[0].Input, &s))
	assert.True(t, strings.HasSuffix(s, "..."))
}

func TestParseChatStream_ByteCap(t *testing.T) {
	var chunks []string
	for i := 0; i < 200; i++ {
		chunks = append(chunks, `{"type":"text-delta","id":"t","delta":"0123456789"}`)
	}
	chunks = append(chunks, `{"type":"finish"}`)
	turn, err := ParseChatStream(strings.NewReader(sse(chunks...)), StreamParserConfig{MaxBytes: 1024})
	require.NoError(t, err)
	assert.True(t, turn.Truncated)
	assert.False(t, turn.Complete)
	assert.Less(t, len(turn.Text), 2000)
}

func TestParseChatStream_PreliminaryOutputKeepsState(t *testing.T) {
	body := sse(
		`{"type":"tool-input-available","toolCallId":"c","toolName":"t","input":{}}`,
		`{"type":"tool-output-available","toolCallId":"c","output":"partial","preliminary":true}`,
		`{"type":"finish"}`,
	)
	turn, err := ParseChatStream(strings.NewReader(body), StreamParserConfig{})
	require.NoError(t, err)
	assert.Equal(t, ToolStateInputAvailable, turn.ToolCalls[0].State)
	assert.JSONEq(t, `"partial"`, string(turn.ToolCalls[0].Output))
}
