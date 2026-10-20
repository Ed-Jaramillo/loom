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
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Span is one span as Phoenix returns it from GET /v1/projects/{p}/spans.
// Attributes keep the names Phoenix stored (OTel gen_ai.* after Loom's OTLP
// export); callers that need Loom names normalize them.
type Span struct {
	// ID is the span's GlobalID, distinct from the OpenTelemetry span ID.
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Context  SpanContext `json:"context"`
	SpanKind string      `json:"span_kind"`
	// ParentID is the OpenTelemetry span ID of the parent, empty for a root.
	ParentID      string                 `json:"parent_id,omitempty"`
	StartTime     time.Time              `json:"start_time"`
	EndTime       time.Time              `json:"end_time"`
	StatusCode    string                 `json:"status_code"`
	StatusMessage string                 `json:"status_message,omitempty"`
	Attributes    map[string]interface{} `json:"attributes"`
	Events        []SpanEvent            `json:"events,omitempty"`
}

// SpanContext carries the OpenTelemetry identifiers.
type SpanContext struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

// SpanEvent is one span event.
type SpanEvent struct {
	Name       string                 `json:"name"`
	Timestamp  time.Time              `json:"timestamp"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
}

// SpanQuery filters GET /v1/projects/{p}/spans. Zero values are omitted.
type SpanQuery struct {
	TraceID    string
	SpanID     string
	ParentID   string
	Name       string
	SpanKind   string
	StatusCode string
	// Attribute is Phoenix's "key:value" filter; the value is JSON-parsed by
	// Phoenix.
	Attribute string
	StartTime time.Time
	EndTime   time.Time
	// Limit is the page size. Zero lets Phoenix choose.
	Limit int
	// MaxPages overrides the client's page cap for this call when positive.
	MaxPages int
}

// SpanPage is the result of ListSpans.
type SpanPage struct {
	Spans []Span
	// Truncated is true when the page cap stopped the listing before Phoenix
	// ran out of results. Callers must not treat a truncated page as the whole
	// trace.
	Truncated bool
	// PagesFetched is how many requests the listing made.
	PagesFetched int
}

func (q SpanQuery) values() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("trace_id", q.TraceID)
	set("span_id", q.SpanID)
	set("parent_id", q.ParentID)
	set("name", q.Name)
	set("span_kind", q.SpanKind)
	set("status_code", q.StatusCode)
	set("attribute", q.Attribute)
	if !q.StartTime.IsZero() {
		v.Set("start_time", q.StartTime.UTC().Format(time.RFC3339Nano))
	}
	if !q.EndTime.IsZero() {
		v.Set("end_time", q.EndTime.UTC().Format(time.RFC3339Nano))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

// ListSpans lists spans in a project, following next_cursor up to the page cap.
func (c *Client) ListSpans(ctx context.Context, project string, q SpanQuery) (SpanPage, error) {
	ctx, end := c.startSpan(ctx, "list_spans")
	var err error
	defer func() { end(err) }()

	if project == "" {
		err = errors.New("phoenix: project is required")
		return SpanPage{}, err
	}
	path := "/v1/projects/" + url.PathEscape(project) + "/spans"
	maxPages := c.maxPages
	if q.MaxPages > 0 {
		maxPages = q.MaxPages
	}

	var page SpanPage
	cursor := ""
	for page.PagesFetched < maxPages {
		values := q.values()
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		var resp struct {
			Data       []Span  `json:"data"`
			NextCursor *string `json:"next_cursor"`
		}
		if err = c.doJSON(ctx, http.MethodGet, path, values, nil, &resp); err != nil {
			return page, err
		}
		page.PagesFetched++
		page.Spans = append(page.Spans, resp.Data...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return page, nil
		}
		cursor = *resp.NextCursor
	}
	page.Truncated = true
	return page, nil
}

// Trace is one row of GET /v1/projects/{p}/traces.
type Trace struct {
	ID                   string    `json:"id"`
	TraceID              string    `json:"trace_id"`
	ProjectID            string    `json:"project_id"`
	StartTime            time.Time `json:"start_time"`
	EndTime              time.Time `json:"end_time"`
	TokenCountPrompt     int64     `json:"token_count_prompt"`
	TokenCountCompletion int64     `json:"token_count_completion"`
	TokenCountTotal      int64     `json:"token_count_total"`
	// Spans is present only when TraceQuery.IncludeSpans is set. It is kept
	// raw because Phoenix's TraceSpanData differs from Span.
	Spans json.RawMessage `json:"spans,omitempty"`
}

// TraceQuery filters GET /v1/projects/{p}/traces. Zero values are omitted.
type TraceQuery struct {
	StartTime         time.Time
	EndTime           time.Time
	Error             *bool
	MinLatencyMs      int
	MaxLatencyMs      int
	SessionIdentifier string
	// Filter is a Phoenix span-filter expression.
	Filter string
	// IncludeSpans asks Phoenix to embed spans. Off by default because the
	// response can be very large.
	IncludeSpans bool
	Limit        int
	MaxPages     int
}

// TracePage is the result of ListTraces.
type TracePage struct {
	Traces       []Trace
	Truncated    bool
	PagesFetched int
}

func (q TraceQuery) values() url.Values {
	v := url.Values{}
	if !q.StartTime.IsZero() {
		v.Set("start_time", q.StartTime.UTC().Format(time.RFC3339Nano))
	}
	if !q.EndTime.IsZero() {
		v.Set("end_time", q.EndTime.UTC().Format(time.RFC3339Nano))
	}
	if q.Error != nil {
		v.Set("error", strconv.FormatBool(*q.Error))
	}
	if q.MinLatencyMs > 0 {
		v.Set("min_latency_ms", strconv.Itoa(q.MinLatencyMs))
	}
	if q.MaxLatencyMs > 0 {
		v.Set("max_latency_ms", strconv.Itoa(q.MaxLatencyMs))
	}
	if q.SessionIdentifier != "" {
		v.Set("session_identifier", q.SessionIdentifier)
	}
	if q.Filter != "" {
		v.Set("filter", q.Filter)
	}
	if q.IncludeSpans {
		v.Set("include_spans", "true")
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

// ListTraces lists traces in a project, following next_cursor up to the page cap.
func (c *Client) ListTraces(ctx context.Context, project string, q TraceQuery) (TracePage, error) {
	ctx, end := c.startSpan(ctx, "list_traces")
	var err error
	defer func() { end(err) }()

	if project == "" {
		err = errors.New("phoenix: project is required")
		return TracePage{}, err
	}
	path := "/v1/projects/" + url.PathEscape(project) + "/traces"
	maxPages := c.maxPages
	if q.MaxPages > 0 {
		maxPages = q.MaxPages
	}

	var page TracePage
	cursor := ""
	for page.PagesFetched < maxPages {
		values := q.values()
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		var resp struct {
			Data       []Trace `json:"data"`
			NextCursor *string `json:"next_cursor"`
		}
		if err = c.doJSON(ctx, http.MethodGet, path, values, nil, &resp); err != nil {
			return page, err
		}
		page.PagesFetched++
		page.Traces = append(page.Traces, resp.Data...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return page, nil
		}
		cursor = *resp.NextCursor
	}
	page.Truncated = true
	return page, nil
}
