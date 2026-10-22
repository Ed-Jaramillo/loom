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
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/communication"
	"go.uber.org/zap"
)

// defaultMaxMessageHops bounds an agent-to-agent exchange started by one user
// turn, so two agents that keep answering each other stop.
const defaultMaxMessageHops = 8

// standaloneDelivery runs turns of loaded agents that are not workflow members
// when messages arrive for them. The zero value is ready to use.
type standaloneDelivery struct {
	mu      sync.Mutex
	running map[string]bool // queue key → a drain worker is running
	// threads maps (recipient, sender, sender's session) to the recipient's
	// session for that exchange. Each delivery also records the reverse entry,
	// so a reply lands in the session that sent the original message. It is
	// in memory: after a restart, a reply starts a new session.
	threads map[threadKey]string
}

type threadKey struct {
	recipient, sender, senderSession string
}

// sessionFor returns the recipient's session for messages from sender's
// senderSession, creating it on first use.
func (d *standaloneDelivery) sessionFor(recipient, sender, senderSession string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.threads == nil {
		d.threads = make(map[threadKey]string)
	}
	k := threadKey{recipient, sender, senderSession}
	if sess, ok := d.threads[k]; ok {
		return sess
	}
	sess := GenerateSessionID()
	d.threads[k] = sess
	if senderSession != "" {
		d.threads[threadKey{sender, recipient, sess}] = senderSession
	}
	return sess
}

func (d *standaloneDelivery) tryStart(queueKey string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running == nil {
		d.running = make(map[string]bool)
	}
	if d.running[queueKey] {
		return false
	}
	d.running[queueKey] = true
	return true
}

func (d *standaloneDelivery) done(queueKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, queueKey)
}

// agentIdentity keys exchange threads by agent, however a message addressed it.
func agentIdentity(ag *agent.Agent, fallback string) string {
	if id := ag.GetID(); id != "" {
		return id
	}
	return fallback
}

// standaloneRecipient returns the loaded agent that messages queued under
// queueKey (its name or GUID) are for, when it is not a workflow member.
func (s *MultiAgentServer) standaloneRecipient(queueKey string) (*agent.Agent, string, bool) {
	if queueKey == "" || strings.Contains(queueKey, ":") {
		return nil, "", false
	}
	ag, guid, err := s.getAgent(queueKey)
	if err != nil || ag == nil {
		return nil, "", false
	}
	// A workflow member shares its agent with the workflow's worker, which
	// already handles its messages.
	if s.registry != nil {
		if info, infoErr := s.registry.GetAgentInfo(guid); infoErr == nil {
			if cfg := s.registry.GetConfig(info.Name); cfg != nil && cfg.Metadata["workflow"] != "" {
				return nil, "", false
			}
		}
	}
	s.workflowSubAgentsMu.RLock()
	defer s.workflowSubAgentsMu.RUnlock()
	for _, wctx := range s.workflowSubAgents {
		if wctx.agent == ag {
			return nil, "", false
		}
	}
	return ag, guid, true
}

// deliverToStandaloneAgent starts a worker that hands queueKey's pending
// messages to its agent, one turn per message. It reports whether queueKey
// belongs to such an agent; a worker already running for it counts.
func (s *MultiAgentServer) deliverToStandaloneAgent(ctx context.Context, queueKey string) bool {
	ag, guid, ok := s.standaloneRecipient(queueKey)
	if !ok {
		return false
	}
	if !s.standalone.tryStart(queueKey) {
		return true
	}
	turnCtx := turnIdentityContext(ctx)
	started := s.goWorker("standalone-delivery", func() {
		defer s.standalone.done(queueKey)
		for ctx.Err() == nil {
			msg, err := s.messageQueue.Dequeue(turnCtx, queueKey)
			if err != nil || msg == nil {
				return
			}
			s.deliverStandaloneMessage(turnCtx, ag, guid, msg)
		}
	})
	if !started {
		s.standalone.done(queueKey)
		return false
	}
	return true
}

func (s *MultiAgentServer) deliverStandaloneMessage(ctx context.Context, ag *agent.Agent, guid string, msg *communication.QueueMessage) {
	hop := communication.MessageHop(msg)
	if hop > defaultMaxMessageHops {
		reason := fmt.Sprintf("hop limit %d reached; the agents kept replying to each other", defaultMaxMessageHops)
		if err := s.messageQueue.Reject(ctx, msg.ID, reason); err != nil {
			s.logger.Warn("Failed to reject message", zap.String("message_id", msg.ID), zap.Error(err))
		}
		return
	}

	sender := msg.FromAgent
	if senderAgent, _, err := s.getAgent(sender); err == nil {
		sender = agentIdentity(senderAgent, sender)
	}
	sessionID := s.standalone.sessionFor(agentIdentity(ag, guid), sender, msg.Metadata[communication.MetadataFromSession])

	content := "[Empty message]"
	if msg.Payload != nil {
		if v := msg.Payload.GetValue(); len(v) > 0 {
			content = string(v)
		} else if ref := msg.Payload.GetReference(); ref != nil {
			content = fmt.Sprintf("[Reference: %s]", ref.Id)
		}
	}
	from := s.agentDisplayName(msg.FromAgent)
	prompt := fmt.Sprintf("[MESSAGE FROM %s]:\n\n%s\n\n(To reply, call send_message with to_agent %q.)", from, content, from)

	s.logger.Info("Delivering message to standalone agent",
		zap.String("message_id", msg.ID),
		zap.String("to_agent", msg.ToAgent),
		zap.String("from_agent", from),
		zap.String("session_id", sessionID),
		zap.Int("hop", hop))

	s.llmSemaphore <- struct{}{}
	_, err := ag.Chat(communication.WithMessageHop(ctx, hop), sessionID, prompt) // #nosec -- intentional: background delivery outlives the sender's request; carries tenant identity only
	<-s.llmSemaphore

	if err != nil {
		s.logger.Warn("Standalone agent turn for message failed",
			zap.String("message_id", msg.ID),
			zap.String("to_agent", msg.ToAgent),
			zap.Error(err))
		if rejErr := s.messageQueue.Reject(ctx, msg.ID, "recipient turn failed: "+err.Error()); rejErr != nil {
			s.logger.Warn("Failed to reject message", zap.String("message_id", msg.ID), zap.Error(rejErr))
		}
		return
	}
	if ackErr := s.messageQueue.Acknowledge(ctx, msg.ID); ackErr != nil {
		s.logger.Warn("Failed to acknowledge message", zap.String("message_id", msg.ID), zap.Error(ackErr))
	}
}
