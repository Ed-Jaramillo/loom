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

package communication

import (
	"context"
	"strconv"
)

// Metadata keys that send_message stamps on every queued message.
const (
	// MetadataFromSession is the sender's session, so a reply can be delivered
	// back into the conversation that asked.
	MetadataFromSession = "loom.from_session"
	// MetadataHop counts agent-to-agent deliveries in one exchange: 1 for a
	// message sent from a user's turn, n+1 for one sent while handling hop n.
	MetadataHop = "loom.hop"
)

type messageHopKey struct{}

// WithMessageHop marks ctx as handling a delivered message at hop.
func WithMessageHop(ctx context.Context, hop int) context.Context {
	return context.WithValue(ctx, messageHopKey{}, hop)
}

// MessageHopFromContext returns the hop of the message ctx is handling, or 0
// for a turn that no delivered message started.
func MessageHopFromContext(ctx context.Context) int {
	hop, _ := ctx.Value(messageHopKey{}).(int)
	return hop
}

// MessageHop returns msg's hop from its metadata; 1 when it has none.
func MessageHop(msg *QueueMessage) int {
	if msg != nil {
		if n, err := strconv.Atoi(msg.Metadata[MetadataHop]); err == nil && n > 0 {
			return n
		}
	}
	return 1
}
