"""Minimal Synaptic Python SDK example.

Run with SD Core listening on localhost:9911 and the dashboard open at
http://localhost:9911/index.html (Docker / Native; SD Core serves the
dashboard). If you're on the static-only Option B fallback via `python
serve.py`, the dashboard is at http://localhost:8765/index.html instead.
You should see a fresh session light up, a tool_call pulse the motor
cortex, and a memory_added neuron pop into the hippocampus.
"""
import time

import synaptic as sd


def main() -> None:
    with sd.connect(adapter_id="hello-brain", model="claude-opus-4-7") as client:
        print(f"connected — session_id={client.session_id}")

        client.emit(
            "prompt_received",
            {"text": "what's the weather", "char_count": 18},
            region_hint="wernicke_area",
        )
        time.sleep(0.3)

        client.emit(
            "tool_call",
            {"tool_name": "bash", "tool_call_id": "tc-1"},
            region_hint="motor_cortex",
        )
        time.sleep(0.3)

        client.emit(
            "tool_result",
            {"tool_name": "bash", "tool_call_id": "tc-1", "ok": True},
            region_hint="cerebellum",
        )
        time.sleep(0.3)

        client.emit(
            "memory_added",
            {"memory_id": "m-7c2", "text": "User asked about the weather"},
            region_hint="hippocampus",
        )
        time.sleep(0.3)

        client.emit(
            "response_complete",
            {"total_tokens": 142, "stop_reason": "end_turn"},
            region_hint="broca_area",
        )

    # session_end is emitted automatically on context exit.
    print("done")


if __name__ == "__main__":
    main()
