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

// Package phoenixread provides read-only Loom tools over Phoenix trace data:
// phoenix_list_traces and phoenix_list_spans. They wrap the narrow REST client
// in pkg/observability/phoenix, so an agent can read traces and spans and
// cannot reach any Phoenix write, delete or admin endpoint.
package phoenixread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teradata-labs/loom/pkg/observability/phoenix"
	"github.com/teradata-labs/loom/pkg/shuttle"
)

// Tool names.
const (
	ListTracesName = "phoenix_list_traces"
	ListSpansName  = "phoenix_list_spans"
)

// Names lists the tools this package provides.
func Names() []string { return []string{ListTracesName, ListSpansName} }

const (
	defaultSpanLimit  = 200
	maxSpanLimit      = 1000
	defaultTraceLimit = 20
	maxTraceLimit     = 100
	// DefaultMaxResultBytes bounds one tool result handed to the model.
	DefaultMaxResultBytes = 64 << 10
	// DefaultMaxValueChars bounds each attribute value.
	DefaultMaxValueChars = 4000
)

// Config configures both tools.
type Config struct {
	// Project is the default Phoenix project (name or ID).
	Project string
	// MaxResultBytes bounds one result. Zero means DefaultMaxResultBytes.
	MaxResultBytes int
	// MaxValueChars bounds each attribute value. Zero means DefaultMaxValueChars.
	MaxValueChars int
}

func (c Config) withDefaults() Config {
	if c.MaxResultBytes <= 0 {
		c.MaxResultBytes = DefaultMaxResultBytes
	}
	if c.MaxValueChars <= 0 {
		c.MaxValueChars = DefaultMaxValueChars
	}
	return c
}

// New returns the named tool, or an error for an unknown name.
func New(name string, client *phoenix.Client, cfg Config) (shuttle.Tool, error) {
	if client == nil {
		return nil, errors.New("phoenixread: phoenix client is required")
	}
	if strings.TrimSpace(cfg.Project) == "" {
		return nil, errors.New("phoenixread: default project is required")
	}
	base := reader{client: client, cfg: cfg.withDefaults(), now: time.Now}
	switch name {
	case ListTracesName:
		return &listTraces{reader: base}, nil
	case ListSpansName:
		return &listSpans{reader: base}, nil
	default:
		return nil, fmt.Errorf("phoenixread: unknown tool %q", name)
	}
}

type reader struct {
	client *phoenix.Client
	cfg    Config
	now    func() time.Time
}

func (r reader) Backend() string { return "" }

func (r reader) project(params map[string]interface{}) string {
	if p, _ := params["project"].(string); strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p)
	}
	return r.cfg.Project
}

func (r reader) fail(start time.Time, code, msg, suggestion string, retryable bool) *shuttle.Result {
	return &shuttle.Result{
		Success:         false,
		Error:           &shuttle.Error{Code: code, Message: msg, Suggestion: suggestion, Retryable: retryable},
		ExecutionTimeMs: r.now().Sub(start).Milliseconds(),
	}
}

func (r reader) ok(start time.Time, data interface{}, meta map[string]interface{}) *shuttle.Result {
	return &shuttle.Result{
		Success:         true,
		Data:            data,
		Metadata:        meta,
		ExecutionTimeMs: r.now().Sub(start).Milliseconds(),
	}
}

func intParam(params map[string]interface{}, key string, def, max int) int {
	v := def
	switch n := params[key].(type) {
	case float64:
		v = int(n)
	case int:
		v = n
	case int64:
		v = int(n)
	}
	if v <= 0 {
		v = def
	}
	if v > max {
		v = max
	}
	return v
}

// --- phoenix_list_traces -------------------------------------------------

type listTraces struct{ reader }

func (t *listTraces) Name() string { return ListTracesName }

func (t *listTraces) Description() string {
	return `List recent traces in a Phoenix project, newest first. Returns trace IDs with start time, duration and token counts.
Filter by session_id to get a session's traces. Use it to find a trace to investigate; then read its spans with phoenix_list_spans. Read-only.`
}

func (t *listTraces) InputSchema() *shuttle.JSONSchema {
	return shuttle.NewObjectSchema(
		"Parameters for phoenix_list_traces",
		map[string]*shuttle.JSONSchema{
			"project":    shuttle.NewStringSchema("Phoenix project name or ID (default: the configured project)"),
			"since":      shuttle.NewStringSchema("Only traces that started within this duration, e.g. \"1h\", \"24h\" (default: no lower bound)"),
			"error_only": shuttle.NewBooleanSchema("Only traces with at least one error span"),
			"session_id": shuttle.NewStringSchema("Only traces of this session: the session.id attribute value (e.g. \"sess_5f2f37ec\") or the session's Phoenix node ID"),
			"limit":      shuttle.NewNumberSchema("Maximum traces to return (default 20, max 100)"),
		},
		nil,
	)
}

// TraceSummary is one row of phoenix_list_traces.
type TraceSummary struct {
	TraceID          string `json:"trace_id"`
	StartTime        string `json:"start_time"`
	DurationMs       int64  `json:"duration_ms"`
	TokensTotal      int64  `json:"tokens_total"`
	TokensPrompt     int64  `json:"tokens_prompt"`
	TokensCompletion int64  `json:"tokens_completion"`
}

func (t *listTraces) Execute(ctx context.Context, params map[string]interface{}) (*shuttle.Result, error) {
	start := t.now()
	q := phoenix.TraceQuery{Limit: intParam(params, "limit", defaultTraceLimit, maxTraceLimit), MaxPages: 1}
	if s, _ := params["since"].(string); strings.TrimSpace(s) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil || d <= 0 {
			return t.fail(start, "INVALID_PARAMS", fmt.Sprintf("since must be a positive duration like \"24h\": %q", s), "", false), nil
		}
		q.StartTime = start.Add(-d)
	}
	if b, ok := params["error_only"].(bool); ok && b {
		q.Error = &b
	}
	if sid, _ := params["session_id"].(string); strings.TrimSpace(sid) != "" {
		q.SessionIdentifier = strings.TrimSpace(sid)
	}
	project := t.project(params)
	page, err := t.client.ListTraces(ctx, project, q)
	if err != nil {
		return t.fail(start, "PHOENIX_UNAVAILABLE", fmt.Sprintf("listing traces in %q: %v", project, err), "Check the project name and that Phoenix is reachable", true), nil
	}
	rows := make([]TraceSummary, 0, len(page.Traces))
	for _, tr := range page.Traces {
		rows = append(rows, TraceSummary{
			TraceID:          tr.TraceID,
			StartTime:        tr.StartTime.UTC().Format(time.RFC3339),
			DurationMs:       tr.EndTime.Sub(tr.StartTime).Milliseconds(),
			TokensTotal:      tr.TokenCountTotal,
			TokensPrompt:     tr.TokenCountPrompt,
			TokensCompletion: tr.TokenCountCompletion,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartTime > rows[j].StartTime })
	return t.ok(start, map[string]interface{}{
		"project": project,
		"traces":  rows,
		"more":    page.Truncated,
	}, map[string]interface{}{"count": len(rows), "read_only": true}), nil
}

// --- phoenix_list_spans --------------------------------------------------

type listSpans struct{ reader }

func (t *listSpans) Name() string { return ListSpansName }

func (t *listSpans) Description() string {
	return `List the spans of one trace in Phoenix (or one span by span_id), in start order, with parent, name, status, duration, attributes and event names.
Attribute names are as Phoenix stores them (OpenTelemetry gen_ai.* for LLM spans: gen_ai.usage.cost, gen_ai.response.finish_reasons, gen_ai.request.max_tokens). Values over a length cap are cut, and the result reports whether spans were left out.
Filter by name or status_code to narrow a large trace. Read-only.`
}

func (t *listSpans) InputSchema() *shuttle.JSONSchema {
	return shuttle.NewObjectSchema(
		"Parameters for phoenix_list_spans",
		map[string]*shuttle.JSONSchema{
			"trace_id":    shuttle.NewStringSchema("OpenTelemetry trace ID, 32 lowercase hex characters").WithPattern("^[0-9a-f]{32}$"),
			"span_id":     shuttle.NewStringSchema("OpenTelemetry span ID, 16 lowercase hex characters, to read one span").WithPattern("^[0-9a-f]{16}$"),
			"project":     shuttle.NewStringSchema("Phoenix project name or ID (default: the configured project)"),
			"name":        shuttle.NewStringSchema("Only spans with this exact name, e.g. \"llm.completion\" or \"agent.tool_execution\""),
			"status_code": shuttle.NewStringSchema("Only spans with this status").WithEnum("OK", "ERROR", "UNSET"),
			"limit":       shuttle.NewNumberSchema("Maximum spans to return (default 200, max 1000)"),
		},
		nil,
	)
}

// SpanRow is one span in phoenix_list_spans.
type SpanRow struct {
	SpanID        string                 `json:"span_id"`
	ParentID      string                 `json:"parent_id,omitempty"`
	Name          string                 `json:"name"`
	Kind          string                 `json:"kind,omitempty"`
	Status        string                 `json:"status"`
	StatusMessage string                 `json:"status_message,omitempty"`
	StartTime     string                 `json:"start_time"`
	DurationMs    int64                  `json:"duration_ms"`
	Attributes    map[string]interface{} `json:"attributes,omitempty"`
	Events        []string               `json:"events,omitempty"`
}

func (t *listSpans) Execute(ctx context.Context, params map[string]interface{}) (*shuttle.Result, error) {
	start := t.now()
	traceID, _ := params["trace_id"].(string)
	traceID = strings.ToLower(strings.TrimSpace(traceID))
	spanID, _ := params["span_id"].(string)
	spanID = strings.ToLower(strings.TrimSpace(spanID))
	if traceID == "" && spanID == "" {
		return t.fail(start, "INVALID_PARAMS", "trace_id or span_id is required", "Pass the 32-hex trace ID shown in Phoenix", false), nil
	}
	if traceID != "" && !phoenix.ValidTraceID(traceID) {
		return t.fail(start, "INVALID_PARAMS", "trace_id must be 32 lowercase hex characters", "", false), nil
	}
	if spanID != "" && !phoenix.ValidSpanID(spanID) {
		return t.fail(start, "INVALID_PARAMS", "span_id must be 16 lowercase hex characters", "", false), nil
	}
	limit := intParam(params, "limit", defaultSpanLimit, maxSpanLimit)
	q := phoenix.SpanQuery{TraceID: traceID, SpanID: spanID, Limit: limit}
	if n, _ := params["name"].(string); strings.TrimSpace(n) != "" {
		q.Name = strings.TrimSpace(n)
	}
	if s, _ := params["status_code"].(string); strings.TrimSpace(s) != "" {
		q.StatusCode = strings.ToUpper(strings.TrimSpace(s))
	}
	project := t.project(params)
	page, err := t.client.ListSpans(ctx, project, q)
	if err != nil {
		return t.fail(start, "PHOENIX_UNAVAILABLE", fmt.Sprintf("listing spans in %q: %v", project, err), "Check the project name and that Phoenix is reachable", true), nil
	}
	if len(page.Spans) == 0 {
		return t.fail(start, "NOT_FOUND", fmt.Sprintf("no spans matched in project %q", project), "Check the IDs and project; Phoenix only has spans after the OTLP flush", false), nil
	}

	spans := page.Spans
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartTime.Before(spans[j].StartTime) })
	more := page.Truncated
	if len(spans) > limit {
		spans, more = spans[:limit], true
	}

	byName := map[string]int{}
	errorsByName := map[string]int{}
	rows := make([]SpanRow, 0, len(spans))
	used := 0
	omitted := 0
	for _, s := range spans {
		byName[s.Name]++
		if s.StatusCode == "ERROR" {
			errorsByName[s.Name]++
		}
		row := t.row(s)
		b, _ := json.Marshal(row)
		if used+len(b) > t.cfg.MaxResultBytes {
			omitted++
			continue
		}
		used += len(b)
		rows = append(rows, row)
	}
	data := map[string]interface{}{
		"project":        project,
		"trace_id":       traceID,
		"span_count":     len(spans),
		"spans":          rows,
		"spans_by_name":  byName,
		"errors_by_name": errorsByName,
		"more":           more,
	}
	if omitted > 0 {
		data["omitted_for_size"] = omitted
		data["note"] = "Some spans were left out to stay under the result size cap; filter by name or status_code to see them."
	}
	return t.ok(start, data, map[string]interface{}{"count": len(rows), "read_only": true}), nil
}

func (t *listSpans) row(s phoenix.Span) SpanRow {
	row := SpanRow{
		SpanID:        s.Context.SpanID,
		ParentID:      s.ParentID,
		Name:          s.Name,
		Kind:          s.SpanKind,
		Status:        s.StatusCode,
		StatusMessage: clip(s.StatusMessage, t.cfg.MaxValueChars),
		StartTime:     s.StartTime.UTC().Format(time.RFC3339Nano),
		DurationMs:    s.EndTime.Sub(s.StartTime).Milliseconds(),
	}
	if len(s.Attributes) > 0 {
		row.Attributes = make(map[string]interface{}, len(s.Attributes))
		for k, v := range s.Attributes {
			row.Attributes[k] = clipValue(v, t.cfg.MaxValueChars)
		}
	}
	for _, e := range s.Events {
		row.Events = append(row.Events, e.Name)
	}
	return row
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// clipValue cuts long strings and renders nested values as clipped JSON.
func clipValue(v interface{}, max int) interface{} {
	switch x := v.(type) {
	case string:
		return clip(x, max)
	case float64, bool, nil, int, int64:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return clip(fmt.Sprintf("%v", x), max)
		}
		if len(b) <= max {
			return x
		}
		return clip(string(b), max)
	}
}
