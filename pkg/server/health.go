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
package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/teradata-labs/loom/pkg/agent"
	"github.com/teradata-labs/loom/pkg/types"
	"golang.org/x/sync/errgroup"
)

// providerEntry tracks a unique LLM provider and which agents reference it.
type providerEntry struct {
	provider types.LLMProvider
	agents   []string // agent IDs that use this provider (for diagnostics)
}

// collectUniqueProviders deduplicates providers across all agents by name+model.
// With large deployments (100+ agents sharing the same Bedrock model) this avoids
// sending N identical pings when only one is needed.
func collectUniqueProviders(agents map[string]*agent.Agent) map[string]*providerEntry {
	seen := make(map[string]*providerEntry)
	for agentID, ag := range agents {
		for _, llmProvider := range ag.GetAllRoleLLMs() {
			key := llmProvider.Name() + "/" + llmProvider.Model()
			if seen[key] == nil {
				seen[key] = &providerEntry{provider: llmProvider}
			}
			seen[key].agents = append(seen[key].agents, agentID)
		}
	}
	return seen
}

// pingProvider performs a lightweight reachability check on a single LLM
// provider. It prefers the provider's optional HealthCheck method (a cheap,
// dedicated probe, e.g. Ollama's tags endpoint) over a real Chat "ping"
// completion when available, since a full chat completion is slower, costs
// real tokens, and is more prone to transient failures (rate limits, cold
// starts, proxy blips) that don't reflect the agent's actual liveness.
// Callers (ValidateProviders, GetHealth) share this so all health-check
// call sites benefit consistently as providers add HealthCheck support.
func pingProvider(ctx context.Context, provider types.LLMProvider) error {
	if hc, ok := provider.(types.HealthChecker); ok {
		return hc.HealthCheck(ctx)
	}
	_, err := provider.Chat(ctx, []types.Message{
		{Role: "user", Content: "ping"},
	}, nil)
	return err
}

// ValidateProviders performs a preflight health check on all configured LLM providers.
// It deduplicates providers across agents (many agents often share one provider) and
// runs all checks concurrently, so startup time is bounded by the slowest provider
// rather than O(agents × latency).
//
// Called during server startup to ensure all providers are reachable before serving
// requests. Returns an error if any provider fails the health check.
func ValidateProviders(ctx context.Context, agents map[string]*agent.Agent) error {
	failed, err := CheckProviders(ctx, agents)
	if err != nil {
		return err
	}
	if len(failed) == 0 {
		return nil
	}
	lines := make([]string, 0, len(failed))
	for _, f := range failed {
		lines = append(lines, f.Error())
	}
	sort.Strings(lines)
	return fmt.Errorf("LLM provider preflight check failed:\n  %s", strings.Join(lines, "\n  "))
}

// ProviderFailure is one LLM provider that failed its preflight ping, with
// the IDs of the agents that use it.
type ProviderFailure struct {
	Provider string // "name/model"
	AgentIDs []string
	Err      error
}

func (f ProviderFailure) Error() string {
	return fmt.Sprintf("%s (used by: %s): %v", f.Provider, strings.Join(f.AgentIDs, ", "), f.Err)
}

// CheckProviders pings each distinct provider the agents use, once, and
// returns the ones that failed. It returns an error only when no agent has a
// provider, so a caller can keep serving the agents whose providers work.
func CheckProviders(ctx context.Context, agents map[string]*agent.Agent) ([]ProviderFailure, error) {
	unique := collectUniqueProviders(agents)
	if len(unique) == 0 {
		return nil, fmt.Errorf("no LLM providers configured")
	}

	var (
		mu       sync.Mutex
		failures []ProviderFailure
	)
	g, gCtx := errgroup.WithContext(ctx)
	for key, entry := range unique {
		g.Go(func() error {
			checkCtx, cancel := context.WithTimeout(gCtx, 10*time.Second)
			defer cancel()
			if err := pingProvider(checkCtx, entry.provider); err != nil {
				ids := append([]string(nil), entry.agents...)
				sort.Strings(ids)
				mu.Lock()
				failures = append(failures, ProviderFailure{Provider: key, AgentIDs: ids, Err: err})
				mu.Unlock()
			}
			return nil // don't abort other checks on individual failure
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Provider < failures[j].Provider })
	return failures, nil
}

// AgentsFailingPreflight returns the IDs of agents with at least one failed
// provider (any role), sorted.
func AgentsFailingPreflight(failures []ProviderFailure) []string {
	seen := map[string]bool{}
	var ids []string
	for _, f := range failures {
		for _, id := range f.AgentIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}
