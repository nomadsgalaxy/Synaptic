"""Pattern: wrap any function so its calls light up the dashboard.

Useful when you don't want to clutter business code with emit calls. Use
this as a decorator on your tool handlers / agent steps.
"""
from __future__ import annotations

import functools
import time
import uuid
from typing import Any, Callable

import synaptic as sd


def trace_tool(client: sd.Client, region: str = "motor_cortex"):
    """Decorator: each call emits tool_call + tool_result around the function."""

    def decorator(fn: Callable[..., Any]) -> Callable[..., Any]:
        @functools.wraps(fn)
        def wrapper(*args: Any, **kwargs: Any) -> Any:
            tid = f"tc-{uuid.uuid4().hex[:8]}"
            client.emit(
                "tool_call",
                {"tool_name": fn.__name__, "tool_call_id": tid},
                region_hint=region,
            )
            try:
                out = fn(*args, **kwargs)
                client.emit(
                    "tool_result",
                    {"tool_name": fn.__name__, "tool_call_id": tid, "ok": True},
                    region_hint="cerebellum",
                )
                return out
            except Exception as e:
                client.emit(
                    "error",
                    {"where": fn.__name__, "error_message": str(e)[:200]},
                    region_hint="amygdala",
                )
                raise

        return wrapper

    return decorator


def main() -> None:
    client = sd.connect(adapter_id="wrap-example", model="claude-opus-4-7")

    @trace_tool(client, region="parietal_lobe")
    def lookup(symbol: str) -> str:
        time.sleep(0.1)
        return f"price of {symbol}: $42"

    @trace_tool(client, region="temporal_lobe_left")
    def web_search(query: str) -> str:
        time.sleep(0.2)
        return f"first result for: {query}"

    print(lookup("AAPL"))
    print(web_search("synaptic disorder dashboard"))

    client.close()


if __name__ == "__main__":
    main()
