# synaptic-otlp-receiver

Accepts **OTLP/HTTP** trace spans from OpenInference-instrumented AI frameworks and translates each span into a Synaptic brain event in real time.

Supported frameworks via [`openinference-instrumentation-*`](https://github.com/Arize-ai/openinference):

| Framework | Package |
|---|---|
| LangChain | `openinference-instrumentation-langchain` |
| LlamaIndex | `openinference-instrumentation-llama-index` |
| AutoGen | `openinference-instrumentation-autogen` |
| CrewAI | `openinference-instrumentation-crewai` |
| DSPy | `openinference-instrumentation-dspy` |
| OpenAI SDK | `openinference-instrumentation-openai` |
| Anthropic SDK | `openinference-instrumentation-anthropic` |

No third-party Node dependencies — pure Node.js built-ins.

## Quick start

```bash
# 1. Start the receiver
node bridge/otlp-receiver/index.js

# 2. Point your framework at it
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318

# 3. (Python) instrument your framework
pip install openinference-instrumentation-langchain opentelemetry-exporter-otlp-proto-http
```

```python
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from openinference.instrumentation.langchain import LangChainInstrumentor

provider = TracerProvider()
provider.add_span_processor(
    BatchSpanProcessor(OTLPSpanExporter(endpoint="http://localhost:4318/v1/traces"))
)
trace.set_tracer_provider(provider)
LangChainInstrumentor().instrument()

# Now run your LangChain code — the brain lights up automatically
```

## Span kind → SD event mapping

| OpenInference span kind | SD event(s) | Brain region |
|---|---|---|
| `LLM` | `prompt_received` + `response_complete` (with token counts + model name) | Wernicke's Area → Broca's Area |
| `CHAIN` | `subagent_complete` | Prefrontal Cortex |
| `AGENT` | `subagent_complete` | Prefrontal Cortex |
| `TOOL` | `tool_call` + `tool_result` | Motor Cortex → Cerebellum |
| `RETRIEVER` | `memory_recall` | Hippocampus |
| `EMBEDDING` | `memory_added` | Hippocampus |
| `GUARDRAIL` | `error` (only if triggered) | Amygdala |

Spans with unknown or unset `openinference.span.kind` are silently dropped — no noise from internal framework spans.

## Session grouping

The receiver uses `session.id` or `llm.session_id` span attributes (standard OpenInference fields) to group events under one HUD session. If neither is set, spans sharing the same `traceId` are grouped together automatically.

## Configuration

Edit **`config.json`** (same directory as `index.js`):

```json
{
  "SD_CORE_URL": "http://localhost:9911",
  "SD_API_TOKEN": "",
  "OTLP_PORT": "4318"
}
```

All options (env vars take precedence over `config.json`):

| Var | Default | Notes |
|---|---|---|
| `SD_CORE_URL` | `http://localhost:9911` | Full URL of SD Core. Preferred for remote operation. |
| `SD_CORE_HOST` | `127.0.0.1` | Legacy host for localhost mode. |
| `SD_CORE_PORT` | `9911` | Legacy port. |
| `SD_API_TOKEN` | _(empty)_ | Bearer token for SD Core auth. |
| `SD_ADAPTER_ID` | `otlp-receiver` | Shown as the adapter label in the HUD. |
| `OTLP_PORT` | `4318` | Port this receiver listens on (standard OTLP/HTTP port). |

## Verifying it works

```bash
# 1. Receiver running?
curl http://localhost:4318/healthz
# → {"ok":true,"adapter":"otlp-receiver","sd_core":"127.0.0.1:9911"}

# 2. SD Core reachable?
curl http://localhost:9911/healthz

# 3. Post a synthetic LLM span
curl -s -X POST http://localhost:4318/v1/traces \
  -H "Content-Type: application/json" \
  -d '{
    "resourceSpans": [{
      "resource": { "attributes": [{"key":"service.name","value":{"stringValue":"my-agent"}}] },
      "scopeSpans": [{
        "spans": [{
          "traceId": "abc123",
          "spanId": "def456",
          "name": "ChatOpenAI",
          "startTimeUnixNano": "1746580000000000000",
          "endTimeUnixNano":   "1746580002000000000",
          "attributes": [
            {"key":"openinference.span.kind","value":{"stringValue":"LLM"}},
            {"key":"llm.model_name","value":{"stringValue":"gpt-4o"}},
            {"key":"input.value","value":{"stringValue":"What is the capital of France?"}},
            {"key":"llm.token_count.total","value":{"intValue":42}}
          ]
        }]
      }]
    }]
  }'
# → {}   (200 ACK)
# Brain should show prompt_received + response_complete pulses
```

## Troubleshooting

- **No events in dashboard but receiver starts fine.** Check `SD_CORE_URL` in `config.json` matches your running SD Core. Run the healthz check above.
- **Framework connects but spans don't arrive.** Some exporters require `OTEL_EXPORTER_OTLP_PROTOCOL=http/json` to use JSON instead of protobuf. The receiver speaks JSON only.
- **Session IDs don't group correctly.** Make sure your framework sets `session.id` on spans. For LangChain, pass `metadata={"session_id": "..."}` in your chain invocation.
- **Port 4318 already in use.** Set `OTLP_PORT=4319` in `config.json` and update your `OTEL_EXPORTER_OTLP_ENDPOINT` accordingly.
