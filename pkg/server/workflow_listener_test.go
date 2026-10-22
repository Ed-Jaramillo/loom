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

package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/teradata-labs/loom/pkg/agent"
)

// A coordinator registers no queue notification channel; the queue monitor
// notifies it. The queue must still count it as listening.
func TestConfigureCommunication_CoordinatorCountsAsListener(t *testing.T) {
	srv := setupBroadcastTestServer(t, map[string]*agent.Agent{}, nil)

	assert.False(t, srv.messageQueue.HasListener("coord-guid"))

	srv.workflowSubAgentsMu.Lock()
	srv.workflowSubAgents["session-1:wf"] = &workflowSubAgentContext{agentID: "coord-guid"}
	srv.workflowSubAgentsMu.Unlock()

	assert.True(t, srv.messageQueue.HasListener("coord-guid"))
	assert.False(t, srv.messageQueue.HasListener("other-agent"))
}
