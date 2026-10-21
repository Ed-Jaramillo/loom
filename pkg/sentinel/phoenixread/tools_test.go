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

package phoenixread

import (
	"context"
	"encoding/json"
	"fmt"
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

const trace = "be101cb68058bd6137cf51091b08f3fa"

type fakePhoenix struct {
	mu      sync.Mutex
	queries []string
	methods []string
}

func spanJSON(id, parent, name, status string, offsetMs int, attrs string) string {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).Add(time.Duration(offsetMs) * time.Millisecond)
	return fmt.Sprintf(`{"name":%q,"context":{"trace_id":%q,"span_id":%q},"parent_id":%q,"span_kind":"LLM","start_time":%q,"end_time":%q,"status_code":%q,"status_message":"","attributes":%s,"events":[{"name":"exception","timestamp":%q}]}`,
		name, trace, id, parent, start.Format(time.RFC3339Nano), start.Add(250*time.Millisecond).Format(time.RFC3339Nano), status, attrs, start.Format(time.RFC3339Nano))
}

func (f *fakePhoenix) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RequestURI())
		f.methods = append(f.methods, r.Method)
		f.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/projects/broken/"):
			http.Error(w, "internal error", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/spans"):
			// Returned out of order to prove the tool sorts by start time.
			_, _ = fmt.Fprintf(w, `{"data":[%s,%s,%s],"next_cursor":null}`,
				spanJSON("0000000000000002", "0000000000000001", "llm.completion", "OK", 20, `{"gen_ai.usage.cost":0.12,"gen_ai.response.finish_reasons":"max_tokens","gen_ai.prompt":"`+strings.Repeat("p", 2000)+`"}`),
				spanJSON("0000000000000001", "", "agent.chat", "OK", 0, `{"session.id":"sess_1"}`),
				spanJSON("0000000000000003", "0000000000000001", "agent.tool_execution", "ERROR", 40, `{"loom.tool_name":"graph_memory","nested":{"a":[1,2,3]}}`),
			)
		case strings.HasSuffix(r.URL.Path, "/traces"):
			_, _ = fmt.Fprint(w, `{"data":[
				{"id":"T1","trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project_id":"P","start_time":"2026-10-21T01:00:00Z","end_time":"2026-10-21T01:00:05Z","token_count_total":10,"token_count_prompt":7,"token_count_completion":3},
				{"id":"T2","trace_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","project_id":"P","start_time":"2026-10-21T02:00:00Z","end_time":"2026-10-21T02:00:01Z","token_count_total":4}
			],"next_cursor":"more"}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func setup(t *testing.T, cfg Config) (*fakePhoenix, *phoenix.Client) {
	t.Helper()
	f := &fakePhoenix{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c, err := phoenix.New(phoenix.Config{BaseURL: srv.URL, APIKey: "k", RetryBackoff: time.Millisecond})
	require.NoError(t, err)
	return f, c
}

func TestNew(t *testing.T) {
	_, c := setup(t, Config{})
	_, err := New(ListSpansName, nil, Config{Project: "p"})
	assert.Error(t, err)
	_, err = New(ListSpansName, c, Config{})
	assert.Error(t, err, "project required")
	_, err = New("phoenix_delete_everything", c, Config{Project: "p"})
	assert.Error(t, err)
	for _, name := range Names() {
		tool, err := New(name, c, Config{Project: "p"})
		require.NoError(t, err)
		assert.Equal(t, name, tool.Name())
		assert.Equal(t, "", tool.Backend())
		assert.Contains(t, tool.Description(), "Read-only")
		_, err = json.Marshal(tool.InputSchema())
		require.NoError(t, err)
	}
}

func TestListSpans(t *testing.T) {
	f, c := setup(t, Config{})
	tool, err := New(ListSpansName, c, Config{Project: "loom-research", MaxValueChars: 100})
	require.NoError(t, err)

	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"trace_id": strings.ToUpper(trace), "status_code": "error", "limit": float64(50),
	})
	require.NoError(t, err)
	require.True(t, res.Success, "%+v", res.Error)

	require.Len(t, f.queries, 1)
	assert.Equal(t, http.MethodGet, f.methods[0])
	q := f.queries[0]
	assert.Contains(t, q, "/v1/projects/loom-research/spans")
	assert.Contains(t, q, "trace_id="+trace)
	assert.Contains(t, q, "status_code=ERROR")
	assert.Contains(t, q, "limit=50")

	data := res.Data.(map[string]interface{})
	rows := data["spans"].([]SpanRow)
	require.Len(t, rows, 3)
	assert.Equal(t, []string{"0000000000000001", "0000000000000002", "0000000000000003"},
		[]string{rows[0].SpanID, rows[1].SpanID, rows[2].SpanID}, "sorted by start time")
	assert.Equal(t, int64(250), rows[0].DurationMs)
	assert.Equal(t, "0000000000000001", rows[1].ParentID)
	assert.Equal(t, 0.12, rows[1].Attributes["gen_ai.usage.cost"])
	prompt := rows[1].Attributes["gen_ai.prompt"].(string)
	assert.Len(t, prompt, 103, "long values are clipped to the cap plus an ellipsis")
	assert.Equal(t, map[string]interface{}{"a": []interface{}{float64(1), float64(2), float64(3)}}, rows[2].Attributes["nested"])
	assert.Equal(t, []string{"exception"}, rows[2].Events)
	assert.Equal(t, map[string]int{"agent.tool_execution": 1}, data["errors_by_name"])
	assert.Equal(t, false, data["more"])
	assert.Equal(t, true, res.Metadata["read_only"])
}

func TestListSpans_SizeCapOmitsAndSays(t *testing.T) {
	_, c := setup(t, Config{})
	tool, err := New(ListSpansName, c, Config{Project: "p", MaxResultBytes: 700})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"trace_id": trace})
	require.NoError(t, err)
	require.True(t, res.Success)
	data := res.Data.(map[string]interface{})
	assert.Greater(t, data["omitted_for_size"], 0)
	assert.Contains(t, data["note"], "filter by name")
	assert.Equal(t, 3, data["span_count"])
}

func TestListSpans_Validation(t *testing.T) {
	f, c := setup(t, Config{})
	tool, err := New(ListSpansName, c, Config{Project: "p"})
	require.NoError(t, err)
	for _, params := range []map[string]interface{}{
		{},
		{"trace_id": "short"},
		{"span_id": "zz"},
	} {
		res, err := tool.Execute(context.Background(), params)
		require.NoError(t, err)
		assert.False(t, res.Success)
		assert.Equal(t, "INVALID_PARAMS", res.Error.Code)
	}
	assert.Empty(t, f.queries, "invalid input makes no Phoenix call")
}

func TestListTraces(t *testing.T) {
	f, c := setup(t, Config{})
	tool, err := New(ListTracesName, c, Config{Project: "loom-research"})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{
		"since": "24h", "error_only": true, "limit": float64(500), "session_id": " sess_5f2f37ec ",
	})
	require.NoError(t, err)
	require.True(t, res.Success, "%+v", res.Error)
	q := f.queries[0]
	assert.Contains(t, q, "/v1/projects/loom-research/traces")
	assert.Contains(t, q, "error=true")
	assert.Contains(t, q, "limit=100", "limit is capped")
	assert.Contains(t, q, "start_time=")
	assert.Contains(t, q, "session_identifier=sess_5f2f37ec")
	data := res.Data.(map[string]interface{})
	rows := data["traces"].([]TraceSummary)
	require.Len(t, rows, 2)
	assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", rows[0].TraceID, "newest first")
	assert.Equal(t, int64(5000), rows[1].DurationMs)
	assert.Equal(t, true, data["more"], "next_cursor present with a one-page cap")

	res, err = tool.Execute(context.Background(), map[string]interface{}{"since": "yesterday"})
	require.NoError(t, err)
	assert.False(t, res.Success)
}

func TestPhoenixErrorsAreRetryableFailures(t *testing.T) {
	_, c := setup(t, Config{})
	tool, err := New(ListSpansName, c, Config{Project: "loom-research"})
	require.NoError(t, err)
	res, err := tool.Execute(context.Background(), map[string]interface{}{"trace_id": trace, "project": "broken"})
	require.NoError(t, err)
	assert.False(t, res.Success)
	assert.Equal(t, "PHOENIX_UNAVAILABLE", res.Error.Code)
	assert.True(t, res.Error.Retryable)
}
