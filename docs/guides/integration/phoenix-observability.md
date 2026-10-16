# Arize Phoenix observability

**Status**: ⚠️ Partial rollout support

- ✅ Loom exports OTLP/HTTP spans with OpenInference attributes, turn trace IDs, nested agent/tool/memory spans, and a local `eval_runs` record.
- ✅ The Phase 5 session-boundary compatibility fix keeps late memory and LLM spans from changing `eval_runs.session_id` ranges.
- ⚠️ Phoenix trace/session annotations and the external experiment-harness telemetry updates from Phase 4 are not implemented in this repository branch. The annotation check below remains a rollout gate.

This guide pins Phoenix 20.22.0 because that is the version used for the recorded baseline. Re-run the local checks before changing the Phoenix version.

## Local verification

Use `examples/config/otlp-phoenix.yaml`. It exports every span, caps exported content at 32,768 runes, redacts configured PII patterns, and writes unredacted Loom-native rows to `./observability.db`.

### Start Phoenix

Docker:

```bash
docker run -d --name phoenix \
  -p 127.0.0.1:6006:6006 \
  -e PHOENIX_WORKING_DIR=/data \
  -v phoenix_data:/data \
  arizephoenix/phoenix:version-20.22.0
curl -fsS http://127.0.0.1:6006/healthz
```

Windows without Docker:

```powershell
python -m pip install arize-phoenix==20.22.0
$env:PHOENIX_HOST = "127.0.0.1"
phoenix serve
```

The pip server also opens OTLP gRPC on TCP 4317. Block inbound access to 4317 before starting it; `PHOENIX_HOST` only controls the HTTP listener.

### Run a three-turn session

Start Loom:

```powershell
looms serve --config examples\config\otlp-phoenix.yaml --http-port 5006
```

Send three requests with the same `session_id`. At least one turn must call an MCP or `fabric` backend tool if the child `mcp.tools.call` or `backend.*` hierarchy is being checked; the built-in `file_read` tool runs in process and does not emit those child spans.

```powershell
$body = @{
  query = "Use an MCP or backend tool to inspect the configured test resource."
  session_id = "phoenix-phase5"
  agent_id = "<agent-id>"
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:5006/v1/weave `
  -ContentType application/json -Body $body
```

Query the exported spans:

```powershell
Invoke-RestMethod http://127.0.0.1:6006/v1/projects/loom-dev/spans |
  ConvertTo-Json -Depth 20 |
  Set-Content phoenix-spans.json
```

Verify:

1. Every `/v1/weave` response has a lowercase 32-hex `traceId` matching a Phoenix `trace_id`.
2. The tree is `server.Weave` (`CHAIN`) → `agent.chat` (`AGENT`) → `agent.conversation_loop` (`CHAIN`). Tool work nests under `agent.tool_execution` (`TOOL`). `graph_memory.extract` nests under the conversation loop and its LLM child has `loom.llm.role = "memory_extract"`.
3. Trace prompt/completion totals equal the sum of the `llm.completion` spans rather than also counting `agent.chat`.
4. Agent, LLM, tool, and retriever spans have `input.value` / `output.value`; exported values obey the 32,768-rune bound.
5. All three traces share `session.id = phoenix-phase5`.
6. Stop Phoenix, send a fourth turn, and confirm the turn completes without a telemetry error. Restart Phoenix, wait for readiness, and confirm a fifth turn exports.
7. `observability.db` receives one `eval_runs` row per completed span, including spans excluded by any OTLP prefix filter and spans completed while Phoenix is unavailable.
8. A judge evaluation and one `CODE` session annotation appear. This check is currently blocked until the Phase 4 Phoenix annotation client and experiment-harness changes are integrated.

For analysis parity, use a single-agent `mastermind_experiment.py --runs 1` run with an agent named `dolly` and SQLite session storage. Do not use `longitudinal_mastermind.py`: it forces in-memory session storage, while the trace reconstruction requires matching Loom and observability SQLite databases.

## Required test gate

Run with FTS5 and the race detector:

```bash
go test -tags fts5 -race \
  ./pkg/observability/... \
  ./pkg/agent/... \
  ./pkg/llm/... \
  ./pkg/server/... \
  ./pkg/task/... \
  ./pkg/evals/judges/... \
  ./cmd/looms/...
go build -tags fts5 ./...
```

If the proto changes, also run:

```bash
buf generate
buf lint
buf breaking --against '.git#branch=main'
```

## VM rollout

Roll out only after the local checks pass and the annotation gate is resolved. Record the source commit, binary SHA-256, Phoenix image tag, and PostgreSQL image tag in the run log.

### PostgreSQL and Phoenix

Create a private Docker network and an alphanumeric password. URL-reserved characters can corrupt `PHOENIX_SQL_DATABASE_URL`, so this setup uses Phoenix's discrete PostgreSQL variables instead.

```bash
docker network create phoenix-net
install -d -m 700 ~/phoenix
password="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)"
cat >~/phoenix/pg.env <<EOF
POSTGRES_USER=phoenix
POSTGRES_DB=phoenix
POSTGRES_PASSWORD=$password
EOF
cat >~/phoenix/phoenix.env <<EOF
PHOENIX_POSTGRES_HOST=phoenix-db
PHOENIX_POSTGRES_PORT=5432
PHOENIX_POSTGRES_USER=phoenix
PHOENIX_POSTGRES_PASSWORD=$password
PHOENIX_POSTGRES_DB=phoenix
EOF
chmod 600 ~/phoenix/pg.env ~/phoenix/phoenix.env
unset password
```

Start pinned images. PostgreSQL port 5432 and Phoenix OTLP gRPC port 4317 are not published.

```bash
docker run -d --name phoenix-db \
  --network phoenix-net \
  --restart unless-stopped \
  --env-file ~/phoenix/pg.env \
  -v phoenix_pg:/var/lib/postgresql/data \
  postgres:16

docker run -d --name phoenix \
  --network phoenix-net \
  --restart unless-stopped \
  -p 127.0.0.1:6006:6006 \
  --env-file ~/phoenix/phoenix.env \
  arizephoenix/phoenix:version-20.22.0
```

Use the same OTLP settings as the example, with `sqlite_path` set to the analysis database path used by the experiment backup process.

### Exposure and backend gates

Before the first experiment:

```bash
ss -ltnp
docker exec phoenix-db psql -U phoenix -d phoenix -c '\dt'
```

Port 6006 must listen only on `127.0.0.1`; 4317 and 5432 must not be published on an external address. From the workstation, ports 6006, 4317, and 5432 must fail without the SSH tunnel. With the tunnel, this must return `OK`:

```powershell
curl.exe http://127.0.0.1:16006/healthz
```

After the diagnostic run, confirm Phoenix wrote spans to PostgreSQL:

```bash
docker exec phoenix-db psql -U phoenix -d phoenix \
  -c 'select count(*) from spans;'
```

Run the one-game diagnostic first, then the 2×2 validation. Compare trace IDs, tree shape, token totals, input/output values, and session grouping with the local result.

## Rollback

Set `observability.mode: embedded` and restart Loom. `eval_runs` continues in SQLite and OTLP export stops. The external experiment harness must treat Phoenix-derived metrics as unavailable rather than fabricating zero values.
