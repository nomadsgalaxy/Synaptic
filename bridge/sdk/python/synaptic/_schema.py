"""Validation against the v1.0 event schema."""
from __future__ import annotations

# Mirrors docs/event-schema.md v1.0. Keep in sync if the schema changes.
EVENT_TYPES = frozenset({
    "session_start",
    "session_end",
    "prompt_received",
    "model_thinking",
    "response_streaming",
    "response_complete",
    "tool_call",
    "tool_result",
    "memory_recall",
    "memory_added",
    "memory_updated",
    "memory_deleted",
    "subagent_spawn",
    "subagent_complete",
    "error",
})


class SchemaError(ValueError):
    """Raised when an event fails v1.0 schema validation."""


def validate_type(event_type: str) -> None:
    if event_type not in EVENT_TYPES:
        raise SchemaError(
            f"Unknown event type {event_type!r}. Valid: {sorted(EVENT_TYPES)}"
        )
