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
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Agent-session routes (/v1/agent_sessions) drive Phoenix's assistant, PXI.
// Shapes follow the Phoenix 20.22.0 OpenAPI document.

// ModelSelection picks the model a session runs on. ProviderType is "builtin"
// (Provider + ModelName) or "custom" (ProviderID + ModelName).
type ModelSelection struct {
	ProviderType string `json:"providerType"`
	// Provider is a Phoenix ModelProvider enum value such as "ANTHROPIC",
	// "OPENAI", "AWS" or "GOOGLE". Builtin only.
	Provider string `json:"provider,omitempty"`
	// ProviderID is the stored custom provider record. Custom only.
	ProviderID string `json:"providerId,omitempty"`
	ModelName  string `json:"modelName"`
}

// Validate checks the selection against the spec's discriminated union.
func (m ModelSelection) Validate() error {
	switch m.ProviderType {
	case "builtin":
		if m.Provider == "" || m.ModelName == "" {
			return errors.New("phoenix: builtin model selection needs provider and modelName")
		}
	case "custom":
		if m.ProviderID == "" || m.ModelName == "" {
			return errors.New("phoenix: custom model selection needs providerId and modelName")
		}
	default:
		return fmt.Errorf("phoenix: model providerType must be builtin or custom, got %q", m.ProviderType)
	}
	return nil
}

// CreateSessionRequest creates an agent session.
type CreateSessionRequest struct {
	Model ModelSelection `json:"model"`
	// Title is optional, at most 100 characters.
	Title string `json:"title,omitempty"`
	// IsEphemeral makes the session expire after inactivity and keeps it out
	// of the user's saved chats.
	IsEphemeral bool `json:"is_ephemeral"`
}

// CreateAgentSession creates a session owned by the API key's user and
// returns its GlobalID.
func (c *Client) CreateAgentSession(ctx context.Context, req CreateSessionRequest) (string, error) {
	ctx, end := c.startSpan(ctx, "create_agent_session")
	var err error
	defer func() { end(err) }()

	if err = req.Model.Validate(); err != nil {
		return "", err
	}
	if len(req.Title) > 100 {
		req.Title = req.Title[:100]
	}
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	err = c.doJSON(ctx, http.MethodPost, "/v1/agent_sessions", nil, req, &resp)
	if err != nil {
		return "", err
	}
	if resp.Data.ID == "" {
		err = errors.New("phoenix: create agent session returned no id")
		return "", err
	}
	return resp.Data.ID, nil
}

// AgentSession is GET /v1/agent_sessions/{id}.
type AgentSession struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	IsEphemeral bool           `json:"is_ephemeral"`
	Model       ModelSelection `json:"model"`
	// IsActive is true while a turn is streaming on the session.
	IsActive bool `json:"is_active"`
	// LastMessageID is the newest persisted transcript message, empty when the
	// transcript is empty.
	LastMessageID string `json:"last_message_id,omitempty"`
}

// GetAgentSession fetches one session.
func (c *Client) GetAgentSession(ctx context.Context, sessionID string) (AgentSession, error) {
	ctx, end := c.startSpan(ctx, "get_agent_session")
	var err error
	defer func() { end(err) }()

	var resp struct {
		Data AgentSession `json:"data"`
	}
	err = c.doJSON(ctx, http.MethodGet, "/v1/agent_sessions/"+url.PathEscape(sessionID), nil, nil, &resp)
	if err != nil {
		return AgentSession{}, err
	}
	return resp.Data, nil
}

// UIMessage is one transcript message. Parts are kept raw because the spec's
// part union is wide; Part helpers decode the fields Loom reads.
type UIMessage struct {
	ID       string            `json:"id"`
	Role     string            `json:"role"`
	Metadata json.RawMessage   `json:"metadata,omitempty"`
	Parts    []json.RawMessage `json:"parts"`
}

// Part is the subset of transcript part fields Loom reads.
type Part struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	State      string          `json:"state,omitempty"`
	ErrorText  string          `json:"errorText,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Approval   *PartApproval   `json:"approval,omitempty"`
}

// PartApproval is a tool part's approval record.
type PartApproval struct {
	ID       string `json:"id"`
	Approved *bool  `json:"approved,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// IsTool reports whether the part is a tool invocation ("tool-<name>" or
// "dynamic-tool").
func (p Part) IsTool() bool {
	return p.Type == "dynamic-tool" || strings.HasPrefix(p.Type, "tool-")
}

// Name returns the tool name for a tool part.
func (p Part) Name() string {
	if p.Type == "dynamic-tool" {
		return p.ToolName
	}
	if strings.HasPrefix(p.Type, "tool-") {
		return strings.TrimPrefix(p.Type, "tool-")
	}
	return ""
}

// DecodedParts decodes the message's parts. Parts that do not decode are
// skipped and counted in the second return value.
func (m UIMessage) DecodedParts() ([]Part, int) {
	out := make([]Part, 0, len(m.Parts))
	skipped := 0
	for _, raw := range m.Parts {
		var p Part
		if err := json.Unmarshal(raw, &p); err != nil || p.Type == "" {
			skipped++
			continue
		}
		out = append(out, p)
	}
	return out, skipped
}

// AssistantMetadata is the "phoenix" namespace of an assistant message's
// metadata.
type AssistantMetadata struct {
	SessionID        string `json:"sessionId"`
	TurnTraceContext *struct {
		TraceID    string `json:"traceId"`
		RootSpanID string `json:"rootSpanId"`
		StartedAt  string `json:"startedAt"`
	} `json:"turnTraceContext,omitempty"`
	Usage *struct {
		Tokens struct {
			Prompt     int64 `json:"prompt"`
			Completion int64 `json:"completion"`
			Total      int64 `json:"total"`
		} `json:"tokens"`
		PromptDetails *struct {
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
		} `json:"promptDetails,omitempty"`
	} `json:"usage,omitempty"`
	Interrupted bool `json:"interrupted"`
}

// AssistantMetadata decodes metadata.phoenix for an assistant message. It
// returns nil when absent or not an assistant payload.
func (m UIMessage) AssistantMetadata() *AssistantMetadata {
	if len(m.Metadata) == 0 {
		return nil
	}
	var wrapper struct {
		Phoenix json.RawMessage `json:"phoenix"`
	}
	if json.Unmarshal(m.Metadata, &wrapper) != nil || len(wrapper.Phoenix) == 0 {
		return nil
	}
	var typed struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(wrapper.Phoenix, &typed) != nil || typed.Type != "assistant" {
		return nil
	}
	var md AssistantMetadata
	if json.Unmarshal(wrapper.Phoenix, &md) != nil {
		return nil
	}
	return &md
}

// ListAgentSessionMessages returns the transcript in order, following
// next_cursor up to the page cap. The bool reports truncation at the cap.
func (c *Client) ListAgentSessionMessages(ctx context.Context, sessionID string) ([]UIMessage, bool, error) {
	ctx, end := c.startSpan(ctx, "list_agent_session_messages")
	var err error
	defer func() { end(err) }()

	path := "/v1/agent_sessions/" + url.PathEscape(sessionID) + "/messages"
	var all []UIMessage
	cursor := ""
	for page := 0; page < c.maxPages; page++ {
		q := url.Values{}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			Data       []UIMessage `json:"data"`
			NextCursor *string     `json:"next_cursor"`
		}
		if err = c.doJSON(ctx, http.MethodGet, path, q, nil, &resp); err != nil {
			return all, false, err
		}
		all = append(all, resp.Data...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return all, false, nil
		}
		cursor = *resp.NextCursor
	}
	return all, true, nil
}

// Chat contexts. The spec's ChatContext union is wide; these are the members
// Loom sends.

// AppContext is the per-turn clock context.
type AppContext struct {
	Type            string `json:"type"`
	CurrentDateTime string `json:"currentDateTime"`
	TimeZone        string `json:"timeZone"`
}

// TraceContext points the assistant at one trace.
type TraceContext struct {
	Type          string `json:"type"`
	ProjectNodeID string `json:"projectNodeId"`
	OTelTraceID   string `json:"otelTraceId"`
}

// SpanContextUI points the assistant at one span.
type SpanContextUI struct {
	Type          string `json:"type"`
	ProjectNodeID string `json:"projectNodeId,omitempty"`
	OTelSpanID    string `json:"otelSpanId,omitempty"`
}

// ToggleContext is the shape of web_access and subagents contexts.
type ToggleContext struct {
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

// GraphQLContext is the deprecated mutations opt-in. Loom always sends false.
type GraphQLContext struct {
	Type             string `json:"type"`
	MutationsEnabled bool   `json:"mutationsEnabled"`
}

var (
	traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ValidTraceID reports whether s is a lowercase 32-hex OpenTelemetry trace ID.
func ValidTraceID(s string) bool { return traceIDPattern.MatchString(s) }

// ValidSpanID reports whether s is a lowercase 16-hex OpenTelemetry span ID.
func ValidSpanID(s string) bool { return spanIDPattern.MatchString(s) }

// NewAppContext builds the clock context for now.
func NewAppContext(now time.Time) AppContext {
	return AppContext{
		Type:            "app",
		CurrentDateTime: now.Format(time.RFC3339),
		TimeZone:        "UTC",
	}
}

// NewTraceContext builds a trace context. The trace ID must be 32 lowercase hex.
func NewTraceContext(projectNodeID, traceID string) (TraceContext, error) {
	if projectNodeID == "" {
		return TraceContext{}, errors.New("phoenix: trace context needs projectNodeId")
	}
	if !ValidTraceID(traceID) {
		return TraceContext{}, fmt.Errorf("phoenix: trace id %q is not 32 lowercase hex characters", traceID)
	}
	return TraceContext{Type: "trace", ProjectNodeID: projectNodeID, OTelTraceID: traceID}, nil
}

// DefaultToggleContexts disables web access, subagents and GraphQL mutations.
func DefaultToggleContexts() []any {
	return []any{
		GraphQLContext{Type: "graphql", MutationsEnabled: false},
		ToggleContext{Type: "web_access", Enabled: false},
		ToggleContext{Type: "subagents", Enabled: false},
	}
}

// ToolApproval answers one approval request.
type ToolApproval struct {
	ToolCallID string `json:"toolCallId"`
	Approved   bool   `json:"approved"`
}

// ChatRequest is the body of POST /v1/agent_sessions/{id}/chat.
type ChatRequest struct {
	// Headless selects the terminal/scripted agent configuration.
	Headless bool `json:"headless"`
	// Contexts are the UI contexts for the turn.
	Contexts []any `json:"contexts,omitempty"`
	// EditPermission is "manual" or "bypass". Loom always sends "manual".
	EditPermission string `json:"editPermission"`
	// Model is a precondition: a mismatch with the session returns 409
	// agent_session_model_stale.
	Model ModelSelection `json:"model"`
	// Trigger is always "submit-message".
	Trigger string `json:"trigger"`
	// ID is the client's id for this chat request.
	ID string `json:"id"`
	// Message is the new user message. Omitted for approval continuations.
	Message *UserMessage `json:"message,omitempty"`
	// ToolApprovals answers pending approvals. Cannot be combined with Message.
	ToolApprovals []ToolApproval `json:"toolApprovals,omitempty"`
	// LastMessageID is required once the transcript is non-empty. nil omits it.
	LastMessageID *string `json:"lastMessageId,omitempty"`
	// RecordLocalTraces asks Phoenix to record this turn's own spans into its
	// assistant project, which is how PXI's model spend becomes visible.
	RecordLocalTraces bool `json:"recordLocalTraces"`
}

// UserMessage is the user message Loom appends.
type UserMessage struct {
	ID       string          `json:"id"`
	Role     string          `json:"role"`
	Parts    []TextPart      `json:"parts"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// TextPart is a text message part.
type TextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// NewUserMessage builds a one-part user message with a fresh id.
func NewUserMessage(text string) *UserMessage {
	return &UserMessage{
		ID:    "loom-" + uuid.NewString(),
		Role:  "user",
		Parts: []TextPart{{Type: "text", Text: text}},
	}
}

// Chat runs one turn and parses the stream to completion. The call is bounded
// only by ctx: a PXI turn can take minutes, so callers must set a deadline.
// A 409 is returned as *ConflictError. The returned ChatTurn is non-nil on
// ErrStreamIncomplete so callers can still reread the transcript.
func (c *Client) Chat(ctx context.Context, sessionID string, req ChatRequest, parser StreamParserConfig) (*ChatTurn, error) {
	ctx, end := c.startSpan(ctx, "agent_session_chat")
	var err error
	defer func() { end(err) }()

	if err = req.Model.Validate(); err != nil {
		return nil, err
	}
	if req.Message != nil && len(req.ToolApprovals) > 0 {
		err = errors.New("phoenix: chat request cannot carry both a message and tool approvals")
		return nil, err
	}
	if req.Message == nil && len(req.ToolApprovals) == 0 {
		err = errors.New("phoenix: chat request needs a message or tool approvals")
		return nil, err
	}
	if req.Trigger == "" {
		req.Trigger = "submit-message"
	}
	if req.EditPermission == "" {
		req.EditPermission = "manual"
	}
	if req.ID == "" {
		req.ID = "loom-" + uuid.NewString()
	}
	if parser.MaxBytes <= 0 {
		parser.MaxBytes = c.maxBytes
	}

	body, err := json.Marshal(req)
	if err != nil {
		err = fmt.Errorf("phoenix: encoding chat request: %w", err)
		return nil, err
	}
	path := "/v1/agent_sessions/" + url.PathEscape(sessionID) + "/chat"
	httpReq, err := c.newRequest(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream, application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		err = fmt.Errorf("phoenix: POST %s: %w", path, err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, rerr := c.readBody(resp.Body)
		if rerr != nil {
			err = rerr
			return nil, err
		}
		err = c.statusError(http.MethodPost, path, resp.StatusCode, data)
		return nil, err
	}

	turn, perr := ParseChatStream(resp.Body, parser)
	if perr != nil {
		err = perr
		return turn, err
	}
	return turn, nil
}

// WaitUntilIdle polls the session until no turn is streaming or ctx ends.
// It is used after agent_session_busy before retrying a send.
func (c *Client) WaitUntilIdle(ctx context.Context, sessionID string, poll time.Duration) error {
	if poll <= 0 {
		poll = time.Second
	}
	for {
		s, err := c.GetAgentSession(ctx, sessionID)
		if err != nil {
			return err
		}
		if !s.IsActive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
