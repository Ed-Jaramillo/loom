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

// Package assistant provides ask_phoenix_assistant, a Loom tool that delegates
// a trace question to Phoenix's assistant (PXI) through the agent-session REST
// routes and returns its answer as untrusted data.
//
// The Go side owns the session, the response stream and every approval
// decision. The calling agent only sees question in, answer out. Any tool PXI
// wants to run that needs approval is denied unless its name is on the
// configured read-only allowlist.
package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/teradata-labs/loom/pkg/observability/phoenix"
	"github.com/teradata-labs/loom/pkg/prompts"
	"github.com/teradata-labs/loom/pkg/shuttle"
)

// ToolName is the name the agent calls.
const ToolName = "ask_phoenix_assistant"

const (
	// DefaultTimeout bounds one tool call end to end.
	DefaultTimeout = 120 * time.Second
	// DefaultMaxApprovalRounds bounds how many approval continuations one call
	// may send.
	DefaultMaxApprovalRounds = 4
	// DefaultMaxAnswerBytes bounds the answer text returned to the agent.
	DefaultMaxAnswerBytes = 32 << 10
	// DefaultPayloadBytes bounds each tool input kept in the result.
	DefaultPayloadBytes = 2 << 10
	// busyRetryPoll is how often a busy session is re-checked.
	busyRetryPoll = time.Second
	// transcriptIdlePoll and transcriptSettleTimeout bound the wait for
	// Phoenix to finish saving a turn after its stream ends.
	transcriptIdlePoll      = 250 * time.Millisecond
	transcriptSettleTimeout = 15 * time.Second
)

// Config configures the tool.
type Config struct {
	// Project is the default Phoenix project (name or GlobalID) traces live in.
	Project string
	// Model is the model PXI runs on. Phoenix resolves provider credentials
	// on its side.
	Model phoenix.ModelSelection
	// ApproveTools lists PXI tool names allowed to run when PXI asks for
	// approval. Everything else is denied. Keep it to read-only tools.
	ApproveTools []string
	// Timeout bounds one call. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxApprovalRounds bounds approval continuations. Zero means
	// DefaultMaxApprovalRounds.
	MaxApprovalRounds int
	// MaxAnswerBytes bounds the answer. Zero means DefaultMaxAnswerBytes.
	MaxAnswerBytes int
	// PayloadBytes bounds each kept tool input. Zero means DefaultPayloadBytes.
	PayloadBytes int
	// RecordLocalTraces asks Phoenix to record PXI's own turn as a trace in
	// its assistant project, so PXI's model spend can be read back.
	RecordLocalTraces bool
	// SkipTraceCheck disables listing the trace's spans before a session is
	// created. The listing saves a PXI turn on a wrong trace id and is what
	// PXI's span citations are checked against; with it skipped, citations
	// are reported unchecked.
	SkipTraceCheck bool
	// Prompts optionally supplies the message template sent to PXI under
	// RequestPromptKey. Nil uses the built-in template.
	Prompts prompts.PromptRegistry
	// LoomVersion is added to the Loom context sent with every question.
	LoomVersion string
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxApprovalRounds <= 0 {
		c.MaxApprovalRounds = DefaultMaxApprovalRounds
	}
	if c.MaxAnswerBytes <= 0 {
		c.MaxAnswerBytes = DefaultMaxAnswerBytes
	}
	if c.PayloadBytes <= 0 {
		c.PayloadBytes = DefaultPayloadBytes
	}
	return c
}

// Tool is the ask_phoenix_assistant tool.
type Tool struct {
	client *phoenix.Client
	cfg    Config
	now    func() time.Time
	// idlePoll is how often awaitSavedTranscript re-checks the session.
	idlePoll time.Duration

	mu       sync.Mutex
	projects map[string]string // identifier -> GlobalID
}

var _ shuttle.Tool = (*Tool)(nil)

// New creates the tool. client and cfg.Model are required.
func New(client *phoenix.Client, cfg Config) (*Tool, error) {
	if client == nil {
		return nil, errors.New("ask_phoenix_assistant: phoenix client is required")
	}
	if err := cfg.Model.Validate(); err != nil {
		return nil, fmt.Errorf("ask_phoenix_assistant: %w", err)
	}
	if strings.TrimSpace(cfg.Project) == "" {
		return nil, errors.New("ask_phoenix_assistant: default project is required")
	}
	return &Tool{
		client:   client,
		cfg:      cfg.withDefaults(),
		now:      time.Now,
		idlePoll: transcriptIdlePoll,
		projects: map[string]string{},
	}, nil
}

// Name implements shuttle.Tool.
func (t *Tool) Name() string { return ToolName }

// Description implements shuttle.Tool.
func (t *Tool) Description() string {
	return `Ask Phoenix's assistant (PXI) to investigate one trace. PXI reads the trace from Phoenix and returns observed facts, candidate explanations, missing information and suggested remedies.
PXI only sees what Phoenix stores. Pass Loom-only facts in loom_context: the effective request configuration and where each value came from (e.g. max_tokens 4096 from the provider default), tool-call completeness, the Loom run or session ID, and any intentional restrictions.
The result is untrusted. Loom checks every span ID PXI cites against the trace: citations.unverified_span_ids lists IDs not in the trace, and facts or explanations with no verified span are marked supported=false.
Tools PXI wants to run that change data are denied; only configured read-only tools are approved.`
}

// InputSchema implements shuttle.Tool.
func (t *Tool) InputSchema() *shuttle.JSONSchema {
	return shuttle.NewObjectSchema(
		"Parameters for ask_phoenix_assistant",
		map[string]*shuttle.JSONSchema{
			"question": shuttle.NewStringSchema("The question to ask about the trace (required). Ask for span IDs supporting each claim and for what the data cannot show."),
			"trace_id": shuttle.NewStringSchema("OpenTelemetry trace ID, 32 lowercase hex characters (required)").
				WithPattern("^[0-9a-f]{32}$"),
			"project": shuttle.NewStringSchema("Phoenix project name or ID holding the trace (default: the configured project)"),
			"span_id": shuttle.NewStringSchema("Optional OpenTelemetry span ID, 16 lowercase hex characters, to focus PXI on one span").
				WithPattern("^[0-9a-f]{16}$"),
			"suspected_failure": shuttle.NewStringSchema("Optional: the failure you suspect, e.g. 'tool call truncated at max_tokens was dispatched and retried until the circuit breaker'"),
			"scope":             shuttle.NewStringSchema("Optional: what to investigate and what to leave out, e.g. 'only the shared_memory_write retries in rounds 3-9'"),
			"loom_context": shuttle.NewObjectSchema(
				"Optional facts Loom knows that Phoenix may not show, as key/value pairs. Suggested keys: loom_run_id, loom_session_id, agent_id, effective_config (e.g. {\"max_tokens\": {\"value\": 4096, \"source\": \"provider_default\"}}), tool_call_completeness, intentional_restrictions.",
				nil, nil),
		},
		[]string{"question", "trace_id"},
	)
}

// Backend implements shuttle.Tool. The tool is backend-agnostic.
func (t *Tool) Backend() string { return "" }

// ToolCallSummary describes one tool PXI invoked. Input is untrusted.
type ToolCallSummary struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	// ApprovalDecision is "approved", "denied" or "" when no approval was asked.
	ApprovalDecision string          `json:"approval_decision,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	InputTruncated   bool            `json:"input_truncated,omitempty"`
	ErrorText        string          `json:"error_text,omitempty"`
}

// Usage sums PXI's reported token usage across the assistant messages.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// Reported is false when no assistant message carried usage metadata.
	Reported bool `json:"reported"`
}

// Answer is the tool's result payload.
type Answer struct {
	// Note states the trust level of Answer.
	Note string `json:"note"`
	// Answer is PXI's final text. Untrusted.
	Answer          string `json:"answer"`
	AnswerTruncated bool   `json:"answer_truncated,omitempty"`

	TraceID       string `json:"trace_id"`
	SpanID        string `json:"span_id,omitempty"`
	Project       string `json:"project"`
	ProjectNodeID string `json:"project_node_id"`
	SessionID     string `json:"phoenix_session_id"`
	Model         string `json:"model"`

	ToolCalls      []ToolCallSummary `json:"tool_calls,omitempty"`
	DeniedTools    []string          `json:"denied_tools,omitempty"`
	ApprovedTools  []string          `json:"approved_tools,omitempty"`
	ApprovalRounds int               `json:"approval_rounds"`
	// ApprovalsUnresolved is true when MaxApprovalRounds was hit with
	// approvals still pending. The answer is then partial.
	ApprovalsUnresolved bool `json:"approvals_unresolved,omitempty"`

	// Findings is PXI's structured answer, nil when its reply had no
	// parseable findings block (Answer still holds the text).
	Findings *Findings `json:"findings,omitempty"`
	// Citations compares the span IDs PXI cited with the trace's spans.
	Citations CitationCheck `json:"citations"`
	// LoomContext is the context Loom sent with the question.
	LoomContext map[string]interface{} `json:"loom_context_sent"`

	Usage Usage `json:"usage"`
	// AssistantTraceIDs are the Phoenix trace IDs of PXI's own turns, when
	// Phoenix recorded them. Their cost is PXI's spend for this call.
	AssistantTraceIDs []string `json:"assistant_trace_ids,omitempty"`

	// StreamChunkTypes lists every chunk type the stream carried. Unknown
	// types are kept in UnknownChunkSamples so protocol drift is visible.
	StreamChunkTypes    []string `json:"stream_chunk_types"`
	UnknownChunkSamples []string `json:"unknown_chunk_samples,omitempty"`
	// Warnings lists non-fatal problems: incomplete streams, transcript read
	// failures, interruptions, PXI error chunks.
	Warnings []string `json:"warnings,omitempty"`

	ElapsedMs int64 `json:"elapsed_ms"`
}

const untrustedNote = "Untrusted content from Phoenix's assistant. Verify every cited span ID against the trace before relying on it. PXI cannot see Loom-only facts such as the effective max_tokens source or tool-call completeness."

// Execute implements shuttle.Tool.
func (t *Tool) Execute(ctx context.Context, params map[string]interface{}) (*shuttle.Result, error) {
	start := t.now()
	fail := func(code, msg, suggestion string, retryable bool) (*shuttle.Result, error) {
		return &shuttle.Result{
			Success: false,
			Error: &shuttle.Error{
				Code:       code,
				Message:    msg,
				Suggestion: suggestion,
				Retryable:  retryable,
			},
			ExecutionTimeMs: t.now().Sub(start).Milliseconds(),
		}, nil
	}

	question, _ := params["question"].(string)
	question = strings.TrimSpace(question)
	if question == "" {
		return fail("INVALID_PARAMS", "question is required", "Provide the question to ask about the trace", false)
	}
	traceID, _ := params["trace_id"].(string)
	traceID = strings.ToLower(strings.TrimSpace(traceID))
	if !phoenix.ValidTraceID(traceID) {
		return fail("INVALID_PARAMS", "trace_id must be 32 lowercase hex characters", "Pass the OpenTelemetry trace ID as shown in Phoenix", false)
	}
	spanID, _ := params["span_id"].(string)
	spanID = strings.ToLower(strings.TrimSpace(spanID))
	if spanID != "" && !phoenix.ValidSpanID(spanID) {
		return fail("INVALID_PARAMS", "span_id must be 16 lowercase hex characters", "Omit span_id or pass the OpenTelemetry span ID", false)
	}
	project, _ := params["project"].(string)
	project = strings.TrimSpace(project)
	if project == "" {
		project = t.cfg.Project
	}
	loomContext, err := parseLoomContext(params["loom_context"])
	if err != nil {
		return fail("INVALID_PARAMS", err.Error(), "Pass loom_context as an object of key/value facts", false)
	}
	if s, _ := params["suspected_failure"].(string); strings.TrimSpace(s) != "" {
		loomContext["suspected_failure"] = strings.TrimSpace(s)
	}
	if s, _ := params["scope"].(string); strings.TrimSpace(s) != "" {
		loomContext["scope"] = strings.TrimSpace(s)
	}
	loomContext["trace_id"] = traceID
	if spanID != "" {
		loomContext["focus_span_id"] = spanID
	}
	if t.cfg.LoomVersion != "" {
		loomContext["loom_version"] = t.cfg.LoomVersion
	}

	ctx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()

	projectNodeID, err := t.projectNodeID(ctx, project)
	if err != nil {
		return fail("PHOENIX_PROJECT", fmt.Sprintf("resolving project %q: %v", project, err), "Check the project name and the Phoenix API key", true)
	}

	// The trace's span IDs: an existence check before spending a PXI turn,
	// and the reference PXI's citations are checked against.
	var traceSpans map[string]struct{}
	spansTruncated := false
	if !t.cfg.SkipTraceCheck {
		page, err := t.client.ListSpans(ctx, project, phoenix.SpanQuery{TraceID: traceID})
		if err != nil {
			return fail("PHOENIX_UNAVAILABLE", fmt.Sprintf("listing trace spans in Phoenix: %v", err), "Retry when Phoenix is reachable", true)
		}
		if len(page.Spans) == 0 {
			return fail("TRACE_NOT_FOUND", fmt.Sprintf("trace %s has no spans in project %q", traceID, project), "Check the trace ID and project; Phoenix only has spans after the OTLP flush", false)
		}
		traceSpans = make(map[string]struct{}, len(page.Spans))
		for _, s := range page.Spans {
			traceSpans[strings.ToLower(s.Context.SpanID)] = struct{}{}
		}
		spansTruncated = page.Truncated
		if spanID != "" {
			if _, ok := traceSpans[spanID]; !ok && !spansTruncated {
				return fail("SPAN_NOT_FOUND", fmt.Sprintf("span %s is not in trace %s", spanID, traceID), "Omit span_id or pass a span from this trace", false)
			}
		}
	}

	message, err := renderRequest(ctx, t.cfg.Prompts, requestInput{Question: question, LoomContext: loomContext})
	if err != nil {
		return fail("INVALID_PARAMS", err.Error(), "", false)
	}

	answer := &Answer{
		Note:          untrustedNote,
		TraceID:       traceID,
		SpanID:        spanID,
		Project:       project,
		ProjectNodeID: projectNodeID,
		Model:         t.cfg.Model.ModelName,
		LoomContext:   loomContext,
	}

	sessionID, err := t.client.CreateAgentSession(ctx, phoenix.CreateSessionRequest{
		Model:       t.cfg.Model,
		Title:       "loom " + ToolName + " " + traceID[:8],
		IsEphemeral: true,
	})
	if err != nil {
		return fail("PHOENIX_SESSION", fmt.Sprintf("creating assistant session: %v", err), "Check that the configured model provider is set up in Phoenix (Settings -> AI Providers)", true)
	}
	answer.SessionID = sessionID

	traceCtx, err := phoenix.NewTraceContext(projectNodeID, traceID)
	if err != nil {
		return fail("INVALID_PARAMS", err.Error(), "", false)
	}
	contexts := []any{phoenix.NewAppContext(t.now()), traceCtx}
	if spanID != "" {
		contexts = append(contexts, phoenix.SpanContextUI{Type: "span", ProjectNodeID: projectNodeID, OTelSpanID: spanID})
	}
	contexts = append(contexts, phoenix.DefaultToggleContexts()...)

	parser := phoenix.StreamParserConfig{MaxPayloadBytes: t.cfg.PayloadBytes}
	base := phoenix.ChatRequest{
		Headless:          true,
		Contexts:          contexts,
		EditPermission:    "manual",
		Model:             t.cfg.Model,
		RecordLocalTraces: t.cfg.RecordLocalTraces,
	}

	first := base
	first.Message = phoenix.NewUserMessage(message)
	turn, err := t.chatWithBusyRetry(ctx, sessionID, first, parser)
	if err != nil && !errors.Is(err, phoenix.ErrStreamIncomplete) {
		return fail("PHOENIX_CHAT", fmt.Sprintf("assistant turn failed: %v", err), "Retry; if the error is a 409 model mismatch, fix the configured model", true)
	}
	turns := []*phoenix.ChatTurn{turn}
	if err != nil {
		answer.Warnings = append(answer.Warnings, "first turn: "+err.Error())
	}

	// Approval loop. The transcript is authoritative for pending approvals;
	// the stream is a hint.
	decisions := map[string]string{} // toolCallId -> approved|denied
	for round := 0; round < t.cfg.MaxApprovalRounds; round++ {
		pending, lastID, warn := t.pendingApprovals(ctx, sessionID, turns[len(turns)-1])
		if warn != "" {
			answer.Warnings = append(answer.Warnings, warn)
		}
		if len(pending) == 0 {
			break
		}
		if lastID == "" {
			answer.Warnings = append(answer.Warnings, "approvals pending but no trailing assistant message id; cannot continue")
			answer.ApprovalsUnresolved = true
			break
		}
		approvals := make([]phoenix.ToolApproval, 0, len(pending))
		for _, p := range pending {
			approved := t.approved(p.ToolName)
			approvals = append(approvals, phoenix.ToolApproval{ToolCallID: p.ToolCallID, Approved: approved})
			if approved {
				decisions[p.ToolCallID] = "approved"
				answer.ApprovedTools = appendUnique(answer.ApprovedTools, p.ToolName)
			} else {
				decisions[p.ToolCallID] = "denied"
				answer.DeniedTools = appendUnique(answer.DeniedTools, p.ToolName)
			}
		}
		answer.ApprovalRounds++
		cont := base
		cont.ToolApprovals = approvals
		cont.LastMessageID = &lastID
		next, err := t.chatWithBusyRetry(ctx, sessionID, cont, parser)
		if err != nil && !errors.Is(err, phoenix.ErrStreamIncomplete) {
			answer.Warnings = append(answer.Warnings, fmt.Sprintf("approval round %d failed: %v", answer.ApprovalRounds, err))
			answer.ApprovalsUnresolved = true
			break
		}
		if err != nil {
			answer.Warnings = append(answer.Warnings, fmt.Sprintf("approval round %d: %v", answer.ApprovalRounds, err))
		}
		turns = append(turns, next)
		if answer.ApprovalRounds == t.cfg.MaxApprovalRounds {
			if still, _, _ := t.pendingApprovals(ctx, sessionID, next); len(still) > 0 {
				answer.ApprovalsUnresolved = true
				answer.Warnings = append(answer.Warnings, "max approval rounds reached with approvals still pending")
			}
		}
	}

	t.finish(ctx, sessionID, turns, decisions, answer)

	// PXI reported errors and produced no text: its model call failed (for
	// example a rejected provider key). Retrying the same question will fail
	// the same way, so report it as a non-retryable error rather than an
	// empty success the agent would retry.
	if strings.TrimSpace(answer.Answer) == "" {
		if errs := assistantErrors(turns); len(errs) > 0 {
			return &shuttle.Result{
				Success: false,
				Error: &shuttle.Error{
					Code:       "PXI_ERROR",
					Message:    "Phoenix's assistant returned an error and no answer: " + errs[0],
					Suggestion: "Check the model provider's credentials and model in Phoenix (Settings -> AI Providers) and sentinel.phoenix.assistant.model in looms.yaml; retrying the same question will not help",
					Retryable:  false,
					Details: map[string]interface{}{
						"phoenix_session_id": sessionID,
						"model":              t.cfg.Model.ModelName,
						"assistant_errors":   errs,
					},
				},
				ExecutionTimeMs: t.now().Sub(start).Milliseconds(),
			}, nil
		}
	}

	answer.Findings = parseFindings(answer.Answer)
	if answer.Findings == nil && answer.Answer != "" {
		answer.Warnings = append(answer.Warnings, "assistant reply had no parseable findings block; only free text is available")
	}
	if traceSpans != nil {
		answer.Citations = checkCitations(answer.Answer, answer.Findings, traceSpans, spansTruncated)
	} else {
		answer.Warnings = append(answer.Warnings, "span citations not checked: trace listing skipped")
	}
	answer.ElapsedMs = t.now().Sub(start).Milliseconds()

	return &shuttle.Result{
		Success: true,
		Data:    answer,
		Metadata: map[string]interface{}{
			"phoenix_session_id":  sessionID,
			"approval_rounds":     answer.ApprovalRounds,
			"denied_tools":        answer.DeniedTools,
			"unverified_span_ids": answer.Citations.UnverifiedSpanIDs,
			"findings_parsed":     answer.Findings != nil,
			"untrusted":           true,
		},
		ExecutionTimeMs: answer.ElapsedMs,
	}, nil
}

// assistantErrors collects the error chunks' text across turns, deduplicated
// in first-seen order.
func assistantErrors(turns []*phoenix.ChatTurn) []string {
	var out []string
	for _, turn := range turns {
		if turn == nil {
			continue
		}
		for _, e := range turn.Errors {
			out = appendUnique(out, e)
		}
	}
	return out
}

// projectNodeID resolves and caches a project's GlobalID.
func (t *Tool) projectNodeID(ctx context.Context, project string) (string, error) {
	t.mu.Lock()
	id, ok := t.projects[project]
	t.mu.Unlock()
	if ok {
		return id, nil
	}
	p, err := t.client.GetProject(ctx, project)
	if err != nil {
		return "", err
	}
	if p.ID == "" {
		return "", errors.New("project has no id")
	}
	t.mu.Lock()
	t.projects[project] = p.ID
	t.mu.Unlock()
	return p.ID, nil
}

// chatWithBusyRetry sends a turn and, on agent_session_busy, waits for the
// session to go idle and retries once.
func (t *Tool) chatWithBusyRetry(ctx context.Context, sessionID string, req phoenix.ChatRequest, parser phoenix.StreamParserConfig) (*phoenix.ChatTurn, error) {
	turn, err := t.client.Chat(ctx, sessionID, req, parser)
	if err == nil || !phoenix.IsConflict(err, phoenix.ConflictSessionBusy) {
		return turn, err
	}
	if werr := t.client.WaitUntilIdle(ctx, sessionID, busyRetryPoll); werr != nil {
		return nil, fmt.Errorf("%w (then waiting for idle: %v)", err, werr)
	}
	return t.client.Chat(ctx, sessionID, req, parser)
}

type pendingCall struct {
	ToolCallID string
	ToolName   string
}

// pendingApprovals reads the transcript's trailing assistant message and
// returns its approval-requested tool calls plus that message's id. When the
// transcript cannot be read it falls back to the stream and returns a warning.
func (t *Tool) pendingApprovals(ctx context.Context, sessionID string, turn *phoenix.ChatTurn) ([]pendingCall, string, string) {
	if warn := t.awaitSavedTranscript(ctx, sessionID); warn != "" {
		return t.streamApprovals(turn, warn)
	}
	msgs, truncated, err := t.client.ListAgentSessionMessages(ctx, sessionID)
	if err != nil || truncated {
		warn := "transcript unavailable; approvals taken from the stream"
		if err != nil {
			warn += ": " + err.Error()
		} else {
			warn += " (transcript truncated at page cap)"
		}
		return t.streamApprovals(turn, warn)
	}
	if len(msgs) == 0 {
		return nil, "", ""
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" {
		return nil, last.ID, ""
	}
	parts, _ := last.DecodedParts()
	var out []pendingCall
	for _, p := range parts {
		if p.IsTool() && p.State == phoenix.ToolStateApprovalRequested {
			out = append(out, pendingCall{ToolCallID: p.ToolCallID, ToolName: p.Name()})
		}
	}
	return out, last.ID, ""
}

// streamApprovals is pendingApprovals' fallback when the transcript cannot be
// trusted: the approval requests and message id the stream announced.
func (t *Tool) streamApprovals(turn *phoenix.ChatTurn, warn string) ([]pendingCall, string, string) {
	if turn == nil {
		return nil, "", warn
	}
	var out []pendingCall
	for _, tc := range turn.PendingApprovals() {
		out = append(out, pendingCall{ToolCallID: tc.ToolCallID, ToolName: tc.ToolName})
	}
	return out, turn.MessageID, warn
}

// awaitSavedTranscript waits until Phoenix releases the session's turn lock.
// Phoenix saves a turn's messages before it releases the lock, so reading the
// transcript as soon as the stream ends can miss the assistant message. It
// returns a warning when the session did not go idle in time.
func (t *Tool) awaitSavedTranscript(ctx context.Context, sessionID string) string {
	ctx, cancel := context.WithTimeout(ctx, transcriptSettleTimeout)
	defer cancel()
	if err := t.client.WaitUntilIdle(ctx, sessionID, t.idlePoll); err != nil {
		return "session did not go idle after the stream ended; transcript may be incomplete: " + err.Error()
	}
	return ""
}

// approved reports whether a PXI tool is on the read-only allowlist.
func (t *Tool) approved(toolName string) bool {
	if toolName == "" {
		return false
	}
	for _, n := range t.cfg.ApproveTools {
		if strings.EqualFold(strings.TrimSpace(n), toolName) {
			return true
		}
	}
	return false
}

// finish assembles the answer from the transcript, falling back to the
// streams when the transcript cannot be read.
func (t *Tool) finish(ctx context.Context, sessionID string, turns []*phoenix.ChatTurn, decisions map[string]string, answer *Answer) {
	chunkTypes := map[string]struct{}{}
	for _, turn := range turns {
		if turn == nil {
			continue
		}
		for _, k := range turn.ChunkTypes() {
			chunkTypes[k] = struct{}{}
		}
		answer.UnknownChunkSamples = append(answer.UnknownChunkSamples, turn.UnknownChunkSamples...)
		for _, e := range turn.Errors {
			answer.Warnings = append(answer.Warnings, "assistant error: "+e)
		}
		if turn.Aborted {
			answer.Warnings = append(answer.Warnings, "assistant turn aborted: "+turn.AbortReason)
		}
		if turn.Truncated {
			answer.Warnings = append(answer.Warnings, "assistant stream truncated at byte cap")
		}
	}
	answer.StreamChunkTypes = sortedKeys(chunkTypes)

	if warn := t.awaitSavedTranscript(ctx, sessionID); warn != "" {
		answer.Warnings = append(answer.Warnings, warn)
	}
	msgs, truncated, err := t.client.ListAgentSessionMessages(ctx, sessionID)
	if err != nil {
		answer.Warnings = append(answer.Warnings, "final transcript unavailable, answer taken from the stream: "+err.Error())
		t.finishFromStreams(turns, decisions, answer)
		return
	}
	if truncated {
		answer.Warnings = append(answer.Warnings, "final transcript truncated at page cap")
	}

	var text strings.Builder
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		if md := m.AssistantMetadata(); md != nil {
			if md.Usage != nil {
				answer.Usage.Reported = true
				answer.Usage.PromptTokens += md.Usage.Tokens.Prompt
				answer.Usage.CompletionTokens += md.Usage.Tokens.Completion
				answer.Usage.TotalTokens += md.Usage.Tokens.Total
				if md.Usage.PromptDetails != nil {
					answer.Usage.CacheReadTokens += md.Usage.PromptDetails.CacheRead
					answer.Usage.CacheWriteTokens += md.Usage.PromptDetails.CacheWrite
				}
			}
			if md.TurnTraceContext != nil && md.TurnTraceContext.TraceID != "" {
				answer.AssistantTraceIDs = appendUnique(answer.AssistantTraceIDs, md.TurnTraceContext.TraceID)
			}
			if md.Interrupted {
				answer.Warnings = append(answer.Warnings, "assistant message "+m.ID+" was interrupted")
			}
		}
		parts, skipped := m.DecodedParts()
		if skipped > 0 {
			answer.Warnings = append(answer.Warnings, fmt.Sprintf("%d undecodable parts in message %s", skipped, m.ID))
		}
		for _, p := range parts {
			switch {
			case p.Type == "text":
				if text.Len() > 0 && p.Text != "" {
					text.WriteString("\n")
				}
				text.WriteString(p.Text)
			case p.IsTool():
				input, truncated := capRaw(p.Input, t.cfg.PayloadBytes)
				answer.ToolCalls = append(answer.ToolCalls, ToolCallSummary{
					ToolCallID:       p.ToolCallID,
					Name:             p.Name(),
					State:            p.State,
					ApprovalDecision: decisions[p.ToolCallID],
					Input:            input,
					InputTruncated:   truncated,
					ErrorText:        p.ErrorText,
				})
			}
		}
	}
	answer.Answer, answer.AnswerTruncated = clip(text.String(), t.cfg.MaxAnswerBytes)
	if answer.Answer == "" {
		if streamed := streamText(turns); streamed != "" {
			answer.Answer, answer.AnswerTruncated = clip(streamed, t.cfg.MaxAnswerBytes)
			answer.Warnings = append(answer.Warnings, "transcript had no assistant text; answer taken from the stream")
		} else {
			answer.Warnings = append(answer.Warnings, "assistant returned no text")
		}
	}
}

// streamText joins the assistant text the turns' streams carried.
func streamText(turns []*phoenix.ChatTurn) string {
	var text strings.Builder
	for _, turn := range turns {
		if turn == nil || turn.Text == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteString("\n")
		}
		text.WriteString(turn.Text)
	}
	return text.String()
}

// finishFromStreams is the fallback when the transcript is unreadable.
func (t *Tool) finishFromStreams(turns []*phoenix.ChatTurn, decisions map[string]string, answer *Answer) {
	for _, turn := range turns {
		if turn == nil {
			continue
		}
		for _, tc := range turn.ToolCalls {
			answer.ToolCalls = append(answer.ToolCalls, ToolCallSummary{
				ToolCallID:       tc.ToolCallID,
				Name:             tc.ToolName,
				State:            tc.State,
				ApprovalDecision: decisions[tc.ToolCallID],
				Input:            tc.Input,
				InputTruncated:   tc.InputTruncated,
				ErrorText:        tc.ErrorText,
			})
		}
	}
	answer.Answer, answer.AnswerTruncated = clip(streamText(turns), t.cfg.MaxAnswerBytes)
}

func capRaw(raw json.RawMessage, max int) (json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if len(raw) <= max {
		return append(json.RawMessage(nil), raw...), false
	}
	s, _ := json.Marshal(string(raw[:max]) + "...")
	return s, true
}

func clip(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return s[:max] + "...", true
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
