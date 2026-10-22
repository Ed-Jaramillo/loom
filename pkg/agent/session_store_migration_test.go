// Copyright © 2026 Teradata Corporation - All Rights Reserved.
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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/teradata-labs/loom/pkg/observability"
)

// Reopening a database must not rerun column migrations. messages.agent_id
// used to be checked under its map key "message_agent_id", so every start
// after the first re-ran its ALTER and recorded "duplicate column name".
func TestInitSchema_SecondStartRecordsNoMigrationError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")

	first, err := NewSessionStore(path, observability.NewNoOpTracer())
	require.NoError(t, err)

	var columns, indexes int
	require.NoError(t, first.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='agent_id'`).Scan(&columns))
	require.NoError(t, first.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_agent'`).Scan(&indexes))
	assert.Equal(t, 1, columns, "the first start adds messages.agent_id")
	assert.Equal(t, 1, indexes, "and its index")
	require.NoError(t, first.Close())

	tracer := observability.NewMockTracer()
	second, err := NewSessionStore(path, tracer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	span := tracer.GetSpanByName("session_store.init_schema")
	require.NotNil(t, span)
	assert.NotEqual(t, observability.StatusError, span.Status.Code, "second start recorded: %s", span.Status.Message)
}
