---
title: "Sentinel Phoenix Access Architecture"
weight: 60
---

# Sentinel Phoenix Access Architecture

Sentinel is Loom's trace-debugging meta-agent. This document covers the part that exists: how Loom's Go code and Loom agents reach Arize Phoenix, and how a Loom agent delegates a trace question to Phoenix's assistant (PXI) without ever holding Phoenix credentials or approving a write.

**Version**: v1.4.0
**Status**: ⚠️ Partial. REST client and `ask_phoenix_assistant` implemented with tests against a scripted Phoenix 20.22.0; not yet run against a live Phoenix. Annotation write-back, fixture export and `TraceReader` are 📋 planned.

**Target Audience**: Architects, advanced developers

---

## Table of Contents

- [Design Goals](#design-goals)
- [System Context](#system-context)
- [Architecture Overview](#architecture-overview)
- [Components](#components)
- [Key Interactions](#key-interactions)
- [Data Structures](#data-structures)
- [Design Trade-offs](#design-trade-offs)
- [Constraints and Limitations](#constraints-and-limitations)
- [Concurrency Model](#concurrency-model)
- [Error Handling](#error-handling)
- [Security Considerations](#security-considerations)
- [Evolution](#evolution)
- [Further Reading](#further-reading)

---

## Design Goals

- **Least capability per caller.** Go code gets a client whose method set is the whole of what it can do. The LLM never gets REST access at all, because Phoenix's REST API also deletes spans, traces and projects and manages users and API keys.
- **Writes need a human.** PXI runs with `editPermission: manual`. Loom answers every approval request itself, denying by default, so a Loom agent cannot be talked into approving a Phoenix write through PXI.
- **Untrusted output stays data.** PXI's answer is trace content shaped by a model. It is returned with an explicit trust note, never executed or treated as instructions.
- **Protocol drift is visible.** Phoenix's chat stream has an empty response schema in its OpenAPI document. The parser counts every chunk type and keeps samples of unknown ones, so a Phoenix upgrade shows up in results instead of silently dropping information.
- **Bounded everything.** Timeouts, page caps, body caps, payload caps and approval-round caps, so one question cannot run away in time, memory or PXI spend.

**Non-goals**: replacing Phoenix's UI or storage; detecting failures from PXI's answers (detection is Milestone 2 and uses Loom's own runtime events); multi-tenant scoping.

---

## System Context

```
┌───────────────────────────────────────────────────────────────────────┐
│                               looms                                   │
│                                                                       │
│   [Loom agent]  ──tool call──▶  ask_phoenix_assistant (Go)            │
│        ▲                               │                              │
│        │ answer (untrusted data)       │ narrow REST client           │
│        └───────────────────────────────┤                              │
│                                        │                              │
│   [Loom agents' own spans] ── OTLP ────┼──────────────────────┐       │
└────────────────────────────────────────┼──────────────────────┼───────┘
                                         │                      │
                                         ▼                      ▼
                       ┌────────────────────────────────────────────────┐
                       │                   Phoenix                      │
                       │                                                │
                       │  /v1/projects/{p}/spans   ◀── trace storage    │
                       │  /v1/agent_sessions/*     ──▶ PXI (assistant)  │
                       │                               │                │
                       │                               ├──▶ [LLM provider]
                       │                               │   (Phoenix's key)
                       │                               └──▶ PXI tools   │
                       │                                   (read, write │
                       │                                    w/ approval)│
                       └────────────────────────────────────────────────┘
```

Loom's agents export their traces to Phoenix over OTLP (see [OTLP integration](otlp-integration.md)). Sentinel reads those traces back through two paths: directly by REST for Go code, and indirectly by asking PXI, which has its own tools over the same storage and its own model credentials held by Phoenix.

---

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                     pkg/sentinel/assistant                           │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │  Tool (shuttle.Tool)                                           │  │
│  │   validate ─▶ resolve project ─▶ trace exists? ─▶ session      │  │
│  │   ─▶ chat ─▶ [approval loop] ─▶ assemble Answer                │  │
│  │   policy: approve iff tool ∈ approve_tools, else deny          │  │
│  └───────────────────────────┬────────────────────────────────────┘  │
└──────────────────────────────┼───────────────────────────────────────┘
                               │ typed calls only
┌──────────────────────────────▼───────────────────────────────────────┐
│                   pkg/observability/phoenix                          │
│  ┌──────────────┐  ┌──────────────────┐  ┌────────────────────────┐  │
│  │ Client       │  │ Agent sessions   │  │ Stream parser          │  │
│  │ GetProject   │  │ CreateAgentSess. │  │ SSE "data:" lines      │  │
│  │ ListSpans    │  │ GetAgentSession  │  │ Vercel UI msg chunks   │  │
│  │ ListTraces   │  │ ListMessages     │  │ text / tools / approvals│ │
│  │ Version      │  │ Chat (stream)    │  │ unknown-type sampling  │  │
│  │              │  │ WaitUntilIdle    │  │ byte + payload caps    │  │
│  └──────┬───────┘  └────────┬─────────┘  └────────────────────────┘  │
│         └──────────┬────────┘                                        │
│          doJSON: bearer auth, per-request timeout, body cap,         │
│          GET retry w/ backoff, 409 → ConflictError                   │
└──────────────────────────────────────────────────────────────────────┘
```

There is deliberately no `Do(request)` method on the client. Adding a Phoenix capability means adding a typed method, which is a reviewable change.

---

## Components

### REST client (`pkg/observability/phoenix`)

**Responsibility**: typed, bounded access to the Phoenix 20.22.0 endpoints Loom uses.

**Interface**: `GetProject`, `ListSpans`, `ListTraces`, `Version`, `CreateAgentSession`, `GetAgentSession`, `ListAgentSessionMessages`, `Chat`, `WaitUntilIdle`.

**Invariants**:
- Every request carries `Authorization: Bearer <key>`; the key appears in no log line, error or result.
- A non-streaming request ends within `RequestTimeout`.
- No response body larger than `MaxResponseBytes` is decoded; the call fails with `ErrResponseTooLarge`.
- A listing follows at most `MaxPages` cursors and reports `Truncated` when it stopped early.
- Only GET requests are retried (429, 502, 503, 504, transport errors), so a POST that creates a session or a turn is never duplicated by the client.

### Stream parser

**Responsibility**: turn Phoenix's server-sent event stream into a `ChatTurn`.

Phoenix emits pydantic-ai's Vercel AI UI message stream: one `data: <json>` line per chunk with a `type` field (`start`, `text-delta`, `tool-input-available`, `tool-approval-request`, `tool-output-denied`, `finish`, `error`, `abort`, ...), terminated by `data: [DONE]`. Chunk field names are camelCase.

**Invariants**:
- `Complete` is true only after `finish`, `abort` or `[DONE]`; otherwise `ErrStreamIncomplete` is returned with the partial turn.
- Reads stop at `MaxBytes`; kept tool payloads are cut at `MaxPayloadBytes` and re-encoded as JSON strings so the turn always marshals.
- Every chunk type seen is counted; unknown types are sampled up to a cap.

### Assistant tool (`pkg/sentinel/assistant`)

**Responsibility**: one `ask_phoenix_assistant` call is one ephemeral PXI session answering one question about one trace.

**Policy**: approval requests are answered from the transcript's `approval-requested` tool parts. A tool is approved only when its name is in `approve_tools`; everything else is denied. The decision and the tool name are reported back.

**Invariants**:
- No session is created for an invalid trace ID or a trace with no spans in the project.
- The transcript, not the stream, is the source of the final answer and of pending approvals; the stream is used to wait for the turn and as a fallback.
- At most `MaxApprovalRounds` continuations per call; past that the answer is marked `approvals_unresolved`.

---

## Key Interactions

### One question, one approval round

```
Agent        Tool            Phoenix REST                 PXI
  │            │                  │                         │
  ├─ call ────▶│                  │                         │
  │            ├─ GET project ───▶│                         │
  │            │◀─ node id ───────┤                         │
  │            ├─ GET spans?trace▶│                         │
  │            │◀─ 1 span ────────┤                         │
  │            ├─ POST sessions ─▶│                         │
  │            │◀─ session id ────┤                         │
  │            ├─ POST chat {message, trace ctx, headless}─▶│
  │            │◀─ SSE: text, tool-input, approval-request ─┤
  │            ├─ GET messages ──▶│                         │
  │            │◀─ trailing assistant msg (approval-requested)
  │            │  decide: name ∈ approve_tools ? approve : deny
  │            ├─ POST chat {toolApprovals, lastMessageId}─▶│
  │            │◀─ SSE: tool-output | tool-output-denied, text, finish
  │            ├─ GET messages ──▶│                         │
  │            │◀─ full transcript┤                         │
  │◀─ Answer ──┤  (text, tool calls + decisions, usage, PXI trace ids)
  │            │                  │                         │
```

**Properties**: synchronous from the agent's view; bounded by `Timeout`; a `409 agent_session_busy` on either chat is retried once after the session reports idle; `agent_session_model_stale` and the other conflict codes are terminal for the call.

---

## Data Structures

### `ChatTurn`

What one stream yielded: assistant text, tool calls keyed by `toolCallId` with their last state, approval IDs, finish reason, errors, message metadata, a chunk-type histogram, unknown samples, byte count and truncation flag. Tool call order is first-seen order.

### `Answer`

The tool's result. Text plus provenance: project node ID, session ID, model, tool calls with approval decisions, denied and approved tool names, approval rounds, summed usage, PXI's own trace IDs, stream chunk types, warnings and elapsed time. It carries a fixed trust note as its first field.

**Invariant**: `Answer` always marshals to JSON (truncated payloads are stored as strings).

---

## Design Trade-offs

### Decision 1: REST for Go, PXI (or MCP) for the LLM

**Chosen**: Go code uses the narrow REST client; the LLM reaches Phoenix only through the assistant tool or, separately, through Phoenix's MCP server added as a filtered MCP server.

**Rationale**: the REST API's surface includes destructive and administrative endpoints, and Phoenix 20.22.0 offers no scoped API keys. A fixed method set is the only enforceable boundary.

**Alternatives**: give the agent an HTTP tool pointed at Phoenix (rejected: full API surface reachable with the key); build custom retrieval tools over the REST client for the agent (deferred: Milestone 0 decides whether MCP or PXI already covers retrieval).

### Decision 2: Transcript is authoritative, stream is a hint

**Chosen**: after each turn, read `GET .../messages` and act on the trailing assistant message's tool part states.

**Rationale**: the chat response schema is empty in the spec and the stream can end early; the transcript is the state Phoenix persisted and what `lastMessageId` must match. The stream is still parsed to wait for the turn and to capture protocol information.

**Cost**: one extra GET per turn.

### Decision 3: Deny approvals by default

**Chosen**: approve only names on an allowlist that ships empty.

**Rationale**: which PXI tools exist in headless mode, and which of them write, is not yet verified. An empty allowlist makes the first live run observational: the result lists what PXI wanted to do, and an operator chooses what to allow.

**Alternative**: `editPermission: bypass` (rejected: hands Phoenix writes to a model-driven loop).

### Decision 4: Ephemeral sessions

**Chosen**: one `is_ephemeral: true` session per call.

**Rationale**: no state leaks between questions, nothing accumulates in the operator's saved chats, and Phoenix expires the session on its own. Multi-turn investigations happen in the Loom agent, which can call the tool again.

---

## Constraints and Limitations

- **Phoenix version**: written against 20.22.0's OpenAPI document. Older deployments may lack span filters or the agent-session routes; a contract test against `/openapi.json` is planned.
- **Agent-session stability**: the request types reference Vercel AI and pydantic-ai types that Phoenix pins locally. A Phoenix upgrade can change them; `unknown_chunk_samples` is the early signal.
- **No scoped keys**: the configured key can reach Phoenix's DELETE endpoints; only the client's method set prevents it.
- **Feedback loop**: the client's own `phoenix.rest.*` spans are tagged `loom.meta_agent=sentinel` but the OTLP exporter has no exclude filter yet, so they can land in the Phoenix project they describe.
- **Loom-only facts**: PXI cannot see tool-call completeness or the provenance of `max_tokens`; they must be passed in the question until Milestone 1 puts them on spans.

---

## Concurrency Model

The tool is safe for concurrent calls: the only shared state is the project-ID cache behind a mutex. Each call owns its session, context and parser. The client holds no per-request state beyond the `http.Client`, which is concurrency-safe. Tests run under `-race`.

---

## Error Handling

- Configuration and parameter errors fail before any network call.
- Phoenix HTTP errors become `*APIError` (status, method, path, truncated body) or `*ConflictError` (Phoenix's conflict code); `errors.As` and `IsConflict` select on them.
- A stream without a terminal chunk is a warning on the result, not a failure, because the transcript may still hold the turn.
- Transcript read failures fall back to stream content with a warning.
- The tool returns `shuttle.Result{Success: false, Error: …}` with codes `INVALID_PARAMS`, `PHOENIX_PROJECT`, `PHOENIX_UNAVAILABLE`, `TRACE_NOT_FOUND`, `PHOENIX_SESSION`, `PHOENIX_CHAT`, never a Go error for expected failures.

---

## Security Considerations

- **Credential handling**: the key is read from the environment variable named in config at tool construction, held in the client, and never serialized.
- **Trust boundary**: everything in `Answer.answer`, `tool_calls[].input` and `unknown_chunk_samples` originates from Phoenix or a model and is untrusted. The probe agent's prompt says so; the result's `note` says so.
- **Write prevention**: `editPermission: manual` plus deny-by-default approvals; web access, subagents and GraphQL mutations are sent disabled on every turn.
- **Budget**: `Timeout` and `MaxApprovalRounds` bound one call's PXI spend; `record_local_traces` makes that spend readable back from Phoenix via `assistant_trace_ids`.

---

## Evolution

- **Planned**: `AnnotateSpans` / `AnnotateTraces` with `identifier` for idempotent write-back; a `TraceReader` interface with this client as the first implementation and an attribute normalizer from `gen_ai.*` back to Loom names; `looms sentinel export-fixture`; an OTLP exclude filter for `loom.meta_agent` spans; a contract test pinned to Phoenix's spec.
- **Stable**: the tool's parameter names and the top-level `Answer` fields.
- **May change**: `ChatTurn` internals as Phoenix's stream evolves.

---

## Further Reading

- [Phoenix Assistant Guide](../guides/phoenix-assistant.md): configuration and usage
- [Observability Architecture](observability.md): Loom's tracer model
- [OTLP Integration](otlp-integration.md): exporting Loom traces to Phoenix
- Phoenix REST API: `GET /openapi.json` on the Phoenix server ("Arize-Phoenix REST API" 1.0, version 20.22.0 at time of writing)
