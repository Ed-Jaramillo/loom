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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// The chat route streams Vercel AI "UI message stream" chunks as server-sent
// events: each event is one "data: <json>" line and the stream ends with
// "data: [DONE]". Chunk types and fields follow pydantic-ai's
// ui/vercel_ai/response_types.py, which Phoenix 20.22.0 uses verbatim
// (camelCase on the wire).

// Chunk type names the parser understands. Unknown types are counted and kept
// as raw samples so a protocol change is visible instead of silent.
const (
	ChunkStart               = "start"
	ChunkFinish              = "finish"
	ChunkAbort               = "abort"
	ChunkError               = "error"
	ChunkTextStart           = "text-start"
	ChunkTextDelta           = "text-delta"
	ChunkTextEnd             = "text-end"
	ChunkReasoningDelta      = "reasoning-delta"
	ChunkToolInputStart      = "tool-input-start"
	ChunkToolInputDelta      = "tool-input-delta"
	ChunkToolInputAvailable  = "tool-input-available"
	ChunkToolInputError      = "tool-input-error"
	ChunkToolOutputAvailable = "tool-output-available"
	ChunkToolOutputError     = "tool-output-error"
	ChunkToolApprovalRequest = "tool-approval-request"
	ChunkToolOutputDenied    = "tool-output-denied"
	ChunkMessageMetadata     = "message-metadata"
)

// Tool call states as the stream reports them. They mirror the transcript
// part states in the Phoenix spec.
const (
	ToolStateInputStreaming    = "input-streaming"
	ToolStateInputAvailable    = "input-available"
	ToolStateOutputAvailable   = "output-available"
	ToolStateOutputError       = "output-error"
	ToolStateApprovalRequested = "approval-requested"
	ToolStateApprovalResponded = "approval-responded"
	ToolStateOutputDenied      = "output-denied"
)

// ErrStreamIncomplete is returned when the stream ended without a finish,
// abort or [DONE] marker. The turn may still have been persisted; callers
// should reread the transcript.
var ErrStreamIncomplete = errors.New("phoenix: chat stream ended without a terminal chunk")

// StreamToolCall is one tool invocation observed in a chat stream.
type StreamToolCall struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name,omitempty"`
	State      string `json:"state"`
	// ApprovalID is set when Phoenix asked for approval.
	ApprovalID string `json:"approval_id,omitempty"`
	// Input and Output are kept as raw JSON, truncated to the parser's payload
	// cap. They are untrusted content.
	Input            json.RawMessage `json:"input,omitempty"`
	Output           json.RawMessage `json:"output,omitempty"`
	ErrorText        string          `json:"error_text,omitempty"`
	InputTruncated   bool            `json:"input_truncated,omitempty"`
	OutputTruncated  bool            `json:"output_truncated,omitempty"`
	ProviderExecuted bool            `json:"provider_executed,omitempty"`
	Dynamic          bool            `json:"dynamic,omitempty"`
}

// ChatTurn is everything the parser recovered from one chat stream.
type ChatTurn struct {
	// MessageID is the assistant message id from the start chunk.
	MessageID string `json:"message_id,omitempty"`
	// Text is the concatenated assistant text, text parts in stream order.
	Text string `json:"text"`
	// ReasoningChars counts reasoning text; the content itself is dropped.
	ReasoningChars int `json:"reasoning_chars,omitempty"`
	// ToolCalls lists tool invocations in first-seen order.
	ToolCalls []StreamToolCall `json:"tool_calls,omitempty"`
	// FinishReason is from the finish chunk ("stop", "tool-calls", ...).
	FinishReason string `json:"finish_reason,omitempty"`
	// Aborted is set when an abort chunk arrived; AbortReason carries its text.
	Aborted     bool   `json:"aborted,omitempty"`
	AbortReason string `json:"abort_reason,omitempty"`
	// Errors collects error chunks' errorText.
	Errors []string `json:"errors,omitempty"`
	// MessageMetadata is the last message-metadata (or start/finish metadata)
	// payload, raw. Phoenix puts usage and turnTraceContext here.
	MessageMetadata json.RawMessage `json:"message_metadata,omitempty"`
	// ChunkCounts is a histogram of chunk types, including unknown ones.
	ChunkCounts map[string]int `json:"chunk_counts"`
	// UnknownChunkSamples keeps the first few raw chunks of unknown types.
	UnknownChunkSamples []string `json:"unknown_chunk_samples,omitempty"`
	// Complete is true when a finish, abort or [DONE] marker was seen.
	Complete bool `json:"complete"`
	// Bytes is how many bytes of stream were read.
	Bytes int64 `json:"bytes"`
	// Truncated is set when the parser stopped at its byte cap.
	Truncated bool `json:"truncated,omitempty"`
}

// PendingApprovals returns the tool calls still awaiting approval.
func (t *ChatTurn) PendingApprovals() []StreamToolCall {
	var out []StreamToolCall
	for _, tc := range t.ToolCalls {
		if tc.State == ToolStateApprovalRequested {
			out = append(out, tc)
		}
	}
	return out
}

// StreamParserConfig bounds what the parser keeps.
type StreamParserConfig struct {
	// MaxBytes bounds the stream. Zero means DefaultMaxResponseBytes.
	MaxBytes int64
	// MaxPayloadBytes bounds each kept tool input/output. Zero means 4 KiB.
	MaxPayloadBytes int
	// MaxUnknownSamples bounds UnknownChunkSamples. Zero means 8.
	MaxUnknownSamples int
	// MaxLineBytes bounds one SSE line. Zero means 1 MiB.
	MaxLineBytes int
}

const (
	defaultMaxPayloadBytes   = 4 << 10
	defaultMaxUnknownSamples = 8
	defaultMaxLineBytes      = 1 << 20
)

func (c StreamParserConfig) withDefaults() StreamParserConfig {
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxResponseBytes
	}
	if c.MaxPayloadBytes <= 0 {
		c.MaxPayloadBytes = defaultMaxPayloadBytes
	}
	if c.MaxUnknownSamples <= 0 {
		c.MaxUnknownSamples = defaultMaxUnknownSamples
	}
	if c.MaxLineBytes <= 0 {
		c.MaxLineBytes = defaultMaxLineBytes
	}
	return c
}

// rawChunk is the superset of fields the parser reads. Unknown fields are
// ignored by encoding/json.
type rawChunk struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Delta            string          `json:"delta"`
	MessageID        string          `json:"messageId"`
	MessageMetadata  json.RawMessage `json:"messageMetadata"`
	FinishReason     string          `json:"finishReason"`
	Reason           string          `json:"reason"`
	ErrorText        string          `json:"errorText"`
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	ApprovalID       string          `json:"approvalId"`
	Input            json.RawMessage `json:"input"`
	Output           json.RawMessage `json:"output"`
	ProviderExecuted *bool           `json:"providerExecuted"`
	Dynamic          *bool           `json:"dynamic"`
	Preliminary      *bool           `json:"preliminary"`
}

// ParseChatStream reads an SSE chat response to its end and returns what it
// recovered. It returns ErrStreamIncomplete (with the partial turn) when the
// stream ended without a terminal marker, and the partial turn with
// Truncated=true when the byte cap was hit.
func ParseChatStream(r io.Reader, cfg StreamParserConfig) (*ChatTurn, error) {
	cfg = cfg.withDefaults()
	turn := &ChatTurn{ChunkCounts: map[string]int{}}
	index := map[string]int{} // toolCallId -> position in turn.ToolCalls

	limited := &countingReader{r: io.LimitReader(r, cfg.MaxBytes+1)}
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 0, 64<<10), cfg.MaxLineBytes)

	var text strings.Builder
	for sc.Scan() {
		if limited.n > cfg.MaxBytes {
			turn.Truncated = true
			break
		}
		line := sc.Bytes()
		// SSE: ignore comments, event: and id: lines, blank separators.
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			turn.Complete = true
			break
		}
		var ch rawChunk
		if err := json.Unmarshal(payload, &ch); err != nil {
			turn.ChunkCounts["<unparseable>"]++
			turn.addUnknownSample(payload, cfg.MaxUnknownSamples)
			continue
		}
		turn.ChunkCounts[ch.Type]++
		switch {
		case ch.Type == ChunkStart:
			if ch.MessageID != "" {
				turn.MessageID = ch.MessageID
			}
			if len(ch.MessageMetadata) > 0 {
				turn.MessageMetadata = append(json.RawMessage(nil), ch.MessageMetadata...)
			}
		case ch.Type == ChunkTextDelta:
			text.WriteString(ch.Delta)
		case ch.Type == ChunkReasoningDelta:
			turn.ReasoningChars += len(ch.Delta)
		case ch.Type == ChunkTextStart, ch.Type == ChunkTextEnd:
			// Boundaries only; text is accumulated from deltas.
		case ch.Type == ChunkToolInputStart:
			tc := turn.tool(index, ch.ToolCallID)
			tc.State = ToolStateInputStreaming
			tc.ToolName = firstNonEmpty(ch.ToolName, tc.ToolName)
			tc.ProviderExecuted = boolOr(ch.ProviderExecuted, tc.ProviderExecuted)
			tc.Dynamic = boolOr(ch.Dynamic, tc.Dynamic)
		case ch.Type == ChunkToolInputDelta:
			turn.tool(index, ch.ToolCallID)
		case ch.Type == ChunkToolInputAvailable, ch.Type == ChunkToolInputError:
			tc := turn.tool(index, ch.ToolCallID)
			tc.ToolName = firstNonEmpty(ch.ToolName, tc.ToolName)
			tc.Input, tc.InputTruncated = capPayload(ch.Input, cfg.MaxPayloadBytes)
			tc.ProviderExecuted = boolOr(ch.ProviderExecuted, tc.ProviderExecuted)
			tc.Dynamic = boolOr(ch.Dynamic, tc.Dynamic)
			if ch.Type == ChunkToolInputError {
				tc.State = ToolStateOutputError
				tc.ErrorText = ch.ErrorText
			} else {
				tc.State = ToolStateInputAvailable
			}
		case ch.Type == ChunkToolOutputAvailable:
			tc := turn.tool(index, ch.ToolCallID)
			tc.Output, tc.OutputTruncated = capPayload(ch.Output, cfg.MaxPayloadBytes)
			if ch.Preliminary == nil || !*ch.Preliminary {
				tc.State = ToolStateOutputAvailable
			}
		case ch.Type == ChunkToolOutputError:
			tc := turn.tool(index, ch.ToolCallID)
			tc.State = ToolStateOutputError
			tc.ErrorText = ch.ErrorText
		case ch.Type == ChunkToolApprovalRequest:
			tc := turn.tool(index, ch.ToolCallID)
			tc.State = ToolStateApprovalRequested
			tc.ApprovalID = ch.ApprovalID
		case ch.Type == ChunkToolOutputDenied:
			tc := turn.tool(index, ch.ToolCallID)
			tc.State = ToolStateOutputDenied
		case ch.Type == ChunkMessageMetadata:
			if len(ch.MessageMetadata) > 0 {
				turn.MessageMetadata = append(json.RawMessage(nil), ch.MessageMetadata...)
			}
		case ch.Type == ChunkError:
			turn.Errors = append(turn.Errors, ch.ErrorText)
		case ch.Type == ChunkAbort:
			turn.Aborted = true
			turn.AbortReason = ch.Reason
			turn.Complete = true
		case ch.Type == ChunkFinish:
			turn.FinishReason = ch.FinishReason
			if len(ch.MessageMetadata) > 0 {
				turn.MessageMetadata = append(json.RawMessage(nil), ch.MessageMetadata...)
			}
			turn.Complete = true
		case strings.HasPrefix(ch.Type, "data-"),
			ch.Type == "start-step", ch.Type == "finish-step",
			ch.Type == "reasoning-start", ch.Type == "reasoning-end",
			ch.Type == "source-url", ch.Type == "source-document", ch.Type == "file",
			ch.Type == "done":
			// Known, not needed.
		default:
			turn.addUnknownSample(payload, cfg.MaxUnknownSamples)
		}
	}
	turn.Bytes = limited.n
	turn.Text = text.String()
	if err := sc.Err(); err != nil {
		return turn, fmt.Errorf("phoenix: reading chat stream: %w", err)
	}
	if turn.Truncated {
		return turn, nil
	}
	if !turn.Complete {
		return turn, ErrStreamIncomplete
	}
	return turn, nil
}

func (t *ChatTurn) tool(index map[string]int, id string) *StreamToolCall {
	if i, ok := index[id]; ok {
		return &t.ToolCalls[i]
	}
	t.ToolCalls = append(t.ToolCalls, StreamToolCall{ToolCallID: id})
	index[id] = len(t.ToolCalls) - 1
	return &t.ToolCalls[len(t.ToolCalls)-1]
}

func (t *ChatTurn) addUnknownSample(payload []byte, max int) {
	if len(t.UnknownChunkSamples) >= max {
		return
	}
	s := string(payload)
	if len(s) > 512 {
		s = s[:512] + "..."
	}
	t.UnknownChunkSamples = append(t.UnknownChunkSamples, s)
}

// ChunkTypes returns the chunk type names seen, sorted.
func (t *ChatTurn) ChunkTypes() []string {
	out := make([]string, 0, len(t.ChunkCounts))
	for k := range t.ChunkCounts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func capPayload(raw json.RawMessage, max int) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if len(raw) <= max {
		return append(json.RawMessage(nil), raw...), false
	}
	// A truncated JSON document is no longer valid JSON; store it as a JSON
	// string so the result still marshals.
	s, _ := json.Marshal(string(raw[:max]) + "...")
	return s, true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func boolOr(p *bool, cur bool) bool {
	if p == nil {
		return cur
	}
	return *p
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
