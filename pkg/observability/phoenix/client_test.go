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
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/teradata-labs/loom/pkg/observability"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func newTestClient(t *testing.T, h http.Handler, mutate func(*Config)) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := Config{BaseURL: srv.URL, APIKey: "test-key", RetryBackoff: time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	require.NoError(t, err)
	return c, srv
}

func TestNew_Validation(t *testing.T) {
	_, err := New(Config{APIKey: "k"})
	require.Error(t, err)
	_, err = New(Config{BaseURL: "http://x:6006"})
	require.Error(t, err)
	_, err = New(Config{BaseURL: "not a url", APIKey: "k"})
	require.Error(t, err)
	c, err := New(Config{BaseURL: "http://x:6006/", APIKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, "http://x:6006", c.BaseURL())
	assert.Equal(t, DefaultRequestTimeout, c.timeout)
	assert.Equal(t, DefaultMaxPages, c.maxPages)
	assert.Equal(t, DefaultMaxResponseBytes, c.maxBytes)
}

func TestGetProject_SendsBearerAndDecodes(t *testing.T) {
	var gotAuth, gotPath string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"UHJvamVjdDox","name":"default","description":null}}`))
	}), nil)

	p, err := c.GetProject(context.Background(), "default")
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "/v1/projects/default", gotPath)
	assert.Equal(t, Project{ID: "UHJvamVjdDox", Name: "default"}, p)
}

func TestDoJSON_APIErrorNeverLeaksKey(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Forbidden"))
	}), nil)
	_, err := c.GetProject(context.Background(), "default")
	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
	assert.Equal(t, "Forbidden", apiErr.Body)
	assert.NotContains(t, err.Error(), "test-key")
}

func TestDoJSON_ConflictErrorTyped(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"agent_session_busy","message":"another turn holds the lock"}`))
	}), nil)
	_, err := c.GetAgentSession(context.Background(), "s1")
	require.Error(t, err)
	assert.True(t, IsConflict(err, ConflictSessionBusy))
	assert.False(t, IsConflict(err, ConflictModelStale))
	assert.Contains(t, err.Error(), "another turn holds the lock")
}

func TestDoJSON_RetriesGETOn503NotPOST(t *testing.T) {
	var gets, posts int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if atomic.AddInt32(&gets, 1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"p","name":"default"}}`))
			return
		}
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}), nil)

	_, err := c.GetProject(context.Background(), "default")
	require.NoError(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&gets))

	_, err = c.CreateAgentSession(context.Background(), CreateSessionRequest{
		Model: ModelSelection{ProviderType: "builtin", Provider: "ANTHROPIC", ModelName: "m"},
	})
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&posts), "POST must not be retried")
}

func TestDoJSON_ResponseByteCap(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":"p","name":"` + strings.Repeat("n", 200) + `"}}`))
	}), func(c *Config) { c.MaxResponseBytes = 64 })
	_, err := c.GetProject(context.Background(), "default")
	assert.True(t, errors.Is(err, ErrResponseTooLarge))
}

func TestDoJSON_RequestTimeout(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}), func(c *Config) { c.RequestTimeout = 50 * time.Millisecond })
	start := time.Now()
	_, err := c.CreateAgentSession(context.Background(), CreateSessionRequest{
		Model: ModelSelection{ProviderType: "builtin", Provider: "ANTHROPIC", ModelName: "m"},
	})
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
}

func TestListSpans_FollowsCursorUpToCap(t *testing.T) {
	var cursors []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		assert.Equal(t, "abc", r.URL.Query().Get("trace_id"))
		assert.Equal(t, "2", r.URL.Query().Get("limit"))
		next := "next-" + r.URL.Query().Get("cursor")
		_, _ = w.Write([]byte(`{"data":[{"name":"llm.completion","context":{"trace_id":"abc","span_id":"1"},"span_kind":"LLM","start_time":"2026-10-20T00:00:00Z","end_time":"2026-10-20T00:00:01Z","status_code":"OK","attributes":{"gen_ai.usage.cost":0.5}}],"next_cursor":"` + next + `"}`))
	}), nil)

	page, err := c.ListSpans(context.Background(), "default", SpanQuery{TraceID: "abc", Limit: 2, MaxPages: 3})
	require.NoError(t, err)
	assert.True(t, page.Truncated)
	assert.Equal(t, 3, page.PagesFetched)
	assert.Len(t, page.Spans, 3)
	assert.Equal(t, []string{"", "next-", "next-next-"}, cursors)
	assert.Equal(t, 0.5, page.Spans[0].Attributes["gen_ai.usage.cost"])
}

func TestListSpans_StopsWhenNoCursor(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"next_cursor":null}`))
	}), nil)
	page, err := c.ListSpans(context.Background(), "default", SpanQuery{TraceID: "abc"})
	require.NoError(t, err)
	assert.False(t, page.Truncated)
	assert.Equal(t, 1, page.PagesFetched)
	assert.Empty(t, page.Spans)
}

func TestListTraces_QueryEncoding(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		assert.Equal(t, "true", q.Get("error"))
		assert.Equal(t, "true", q.Get("include_spans"))
		assert.Equal(t, "sess", q.Get("session_identifier"))
		assert.Equal(t, "100", q.Get("min_latency_ms"))
		_, _ = w.Write([]byte(`{"data":[{"id":"T1","trace_id":"abc","project_id":"P","start_time":"2026-10-20T00:00:00Z","end_time":"2026-10-20T00:00:01Z","token_count_total":12}],"next_cursor":null}`))
	}), nil)
	yes := true
	page, err := c.ListTraces(context.Background(), "default", TraceQuery{Error: &yes, IncludeSpans: true, SessionIdentifier: "sess", MinLatencyMs: 100})
	require.NoError(t, err)
	require.Len(t, page.Traces, 1)
	assert.Equal(t, int64(12), page.Traces[0].TokenCountTotal)
}

func TestClient_TracerSpans(t *testing.T) {
	tracer := observability.NewMockTracer()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":"p","name":"default"}}`))
	}), func(c *Config) { c.Tracer = tracer })
	_, err := c.GetProject(context.Background(), "default")
	require.NoError(t, err)
	spans := tracer.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "phoenix.rest.get_project", spans[0].Name)
	assert.Equal(t, MetaAgentSentinel, spans[0].Attributes[AttrMetaAgent])
}
