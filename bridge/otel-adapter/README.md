# synaptic-otel

An **OTLP/HTTP receiver** that translates OpenTelemetry traces into Synaptic events. Listens on `:4318/v1/traces`, accepts both `application/json` and `application/x-protobuf` bodies, and forwards each span as one or more SD events to SD Core on `:9911`.

This is the broadest-coverage adapter: any framework with an OpenInference or GenAI semantic-convention instrumentation lights up the dashboard with no code changes — just point your OTLP exporter at this receiver. It's also the only path that surfaces **token-level streaming** when an instrumentation emits per-chunk span events.

## Span → SD event mapping

The receiver inspects each span's attributes following the OpenInference + GenAI conventions, then emits one or more SD events:

| Span signal | SD event(s) |
|---|---|
| Root span (no parent) | `session_start` (model deduced from `gen_ai.request.model` if present; otherwise backfilled from the first child LLM span via the dashboard's `SessionTracker`) |
| `openinference.span.kind=LLM` (or `gen_ai.system` present) | `model_thinking` → `response_complete` (with `prompt_tokens` / `completion_tokens` / `total_tokens` from `gen_ai.usage.*` or `llm.token_count.*`) |
| Span events named `gen_ai.choice` / `gen_ai.content*` / `*streaming*` inside an LLM span | `response_streaming` per event, with token count + delta preview |
| `openinference.span.kind=TOOL` (or `tool.name` / `gen_ai.tool.name` set) | `tool_call` + `tool_result` |
| `openinference.span.kind=RETRIEVER` | `memory_recall` (with `retrieval.score` + `retrieval.document.id`) |
| `openinference.span.kind=EMBEDDING` | `model_thinking` (frontal lobe) |
| `openinference.span.kind=AGENT` (non-root) | `subagent_spawn` + `subagent_complete` |
| `status.code = ERROR` | extra `error` event with `span.status.message` |

Region hints are auto-assigned by span kind (LLM → broca, RETRIEVER → hippocampus, EMBEDDING → frontal lobe, TOOL → motor cortex, AGENT → prefrontal cortex, GUARDRAIL → amygdala). The dashboard infers a region anyway when none is set, so unknown spans don't break anything — they just don't visualize.

## Install

```bash
cd bridge/otel-adapter
npm install
node index.js
# Listening on http://127.0.0.1:4318
```

## Configuration

| Env var | Default | Notes |
|---|---|---|
| `OTEL_LISTEN_HOST` | `127.0.0.1` | Bind host; set to `0.0.0.0` to accept from other containers. |
| `OTEL_LISTEN_PORT` | `4318` | Standard OTLP/HTTP port. |
| `SD_CORE_URL` | unset | Full URL of SD Core (e.g. `https://cognito.example.com`). Preferred for remote operation; takes precedence over host+port. |
| `SD_CORE_HOST` | `127.0.0.1` | Legacy host for localhost mode. |
| `SD_CORE_PORT` | `9911` | Legacy port. |
| `SD_API_TOKEN` | unset | Bearer token sent on every request when set. Required when SD Core was started with `SD_API_TOKEN`. |
| `SD_ADAPTER_ID` | `otel-adapter` | `adapter_id` on every event (lets the HUD distinguish OTel from MCP/Claude Code traffic). |

For remote operation (this OTel receiver runs on one machine, SD Core on another), see [`docs/REMOTE.md`](../../docs/REMOTE.md).

## Wire your app's OTLP exporter

The receiver behaves like a vanilla OTLP/HTTP collector — anything that exports OTel traces over HTTP (JSON or Protobuf) will work.

### Anthropic SDK (Python)

```bash
pip install opentelemetry-instrumentation-anthropic opentelemetry-exporter-otlp
```

```python
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.anthropic import AnthropicInstrumentor

trace.set_tracer_provider(TracerProvider())
trace.get_tracer_provider().add_span_processor(
    BatchSpanProcessor(OTLPSpanExporter(endpoint="http://localhost:4318/v1/traces"))
)
AnthropicInstrumentor().instrument()
```

### OpenAI SDK (Python)

```bash
pip install opentelemetry-instrumentation-openai-v2 opentelemetry-exporter-otlp
```

```python
from opentelemetry.instrumentation.openai_v2 import OpenAIInstrumentor
OpenAIInstrumentor().instrument()
# (TracerProvider + OTLP exporter setup as above)
```

### LangChain / LlamaIndex / AutoGen / CrewAI (OpenInference)

```bash
pip install openinference-instrumentation-langchain
# or: openinference-instrumentation-llama-index, -autogen, -crewai
pip install opentelemetry-exporter-otlp
```

```python
from openinference.instrumentation.langchain import LangChainInstrumentor
LangChainInstrumentor().instrument()
# (Same TracerProvider + OTLP setup as above)
```

### Generic apps using the OTel SDK

Just set the standard OTLP endpoint envs:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=my-app
# Default protocol is http/protobuf; both work.
# To force JSON: export OTEL_EXPORTER_OTLP_PROTOCOL=http/json
```

### Existing OpenTelemetry Collector → relay path

Already running an OTel Collector? Add this OTLP/HTTP exporter to your collector config and route a pipeline at it:

```yaml
exporters:
  otlphttp/synaptic-disorder:
    endpoint: http://localhost:4318
    encoding: proto

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlphttp/synaptic-disorder]
```

You can fan out — keep your existing tracing backend AND mirror to Synaptic.

## Verifying it works

```bash
# 1. Receiver listening?
curl http://127.0.0.1:4318/healthz   # → "ok"

# 2. SD Core reachable?
curl http://127.0.0.1:9911/healthz   # → 200

# 3. Send a synthetic span (JSON)
curl -X POST http://127.0.0.1:4318/v1/traces \
  -H 'content-type: application/json' \
  -d '{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"AAECAwQFBgcICQoLDA0ODw==","spanId":"AAECAwQFBgc=","name":"chat","attributes":[{"key":"openinference.span.kind","value":{"stringValue":"LLM"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-opus-4-7"}},{"key":"gen_ai.usage.input_tokens","value":{"intValue":"100"}},{"key":"gen_ai.usage.output_tokens","value":{"intValue":"50"}}],"status":{"code":1}}]}]}]}'

# 4. SD Core saw it?
curl 'http://127.0.0.1:9911/events?adapter_id=otel-adapter' | jq '.events[0]'
```

## Limits / non-goals (for v1)

- **Metrics + logs** are not handled. Receiver only mounts `/v1/traces`. (The OTLP endpoint typically also serves `/v1/metrics` and `/v1/logs` — they're not relevant to the dashboard.)
- **gRPC** transport is not supported. Use the HTTP exporter on port 4318, not the gRPC exporter on 4317.
- **No persistent storage**. We're a translator; events fly through to SD Core. SD Core's history endpoint (`GET /events`) is the durable view.
- **No span correlation across services**. Each `trace_id` becomes one HUD session, but if your trace fans out across processes, multi-process correlation requires that all processes export to this receiver.

## Privacy

This adapter runs locally and forwards only to localhost. No telemetry is sent anywhere else. Span attribute values are forwarded verbatim — be aware that some instrumentations include prompt/completion text in attributes (`gen_ai.completion`, `gen_ai.prompt`). If that's a concern, configure your instrumentation to drop those attributes before they reach the OTel exporter.

## Files

```
otel-adapter/
├── package.json
├── index.js                            # OTLP receiver + translator
├── proto/                              # Vendored OTLP traces .proto subset (Apache 2.0)
│   └── opentelemetry/proto/...
└── README.md
```

## License

Pending — Open Community License. The vendored OTLP proto files retain the upstream Apache 2.0 license.
