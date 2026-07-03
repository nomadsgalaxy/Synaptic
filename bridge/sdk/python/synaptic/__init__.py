"""
Synaptic Python SDK.

Tiny client for emitting Synaptic activity events from any Python
agent or instrumentation. Three things to know:

    import synaptic as sd

    client = sd.connect(adapter_id="my-agent", model="claude-opus-4-7")
    client.emit("tool_call", {"tool_name": "bash"}, region_hint="motor_cortex")
    client.close()  # emits session_end

The client never blocks: emits queue and a background thread POSTs to
SD Core. If SD Core is down, events are dropped silently.
"""
from ._client import Client, connect
from ._schema import EVENT_TYPES, SchemaError

__all__ = ["Client", "connect", "EVENT_TYPES", "SchemaError"]
__version__ = "0.1.0"
