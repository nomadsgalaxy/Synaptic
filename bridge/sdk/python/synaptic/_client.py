"""Synaptic client — async-by-default, fire-and-forget POST to SD Core."""
from __future__ import annotations

import json
import os
import queue
import threading
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timezone
from typing import Any, Mapping, Optional

from ._schema import validate_type


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace(
        "+00:00", "Z"
    )


class Client:
    """A connected Synaptic client.

    Use :func:`connect` to create one. Every emit is non-blocking — events
    are placed on a small queue and POSTed by a background daemon thread.
    Calls never raise on network errors; if SD Core is offline the events
    are dropped on the floor.
    """

    def __init__(
        self,
        adapter_id: str,
        model: Optional[str] = None,
        host: str = "127.0.0.1",
        port: int = 9911,
        url: Optional[str] = None,
        api_token: Optional[str] = None,
        session_id: Optional[str] = None,
        queue_max: int = 1024,
    ) -> None:
        self._adapter_id = adapter_id
        self._model = model
        # url overrides host+port when set. Strip trailing slash, then join /event.
        if url:
            self._url = url.rstrip("/") + "/event"
        else:
            self._url = f"http://{host}:{port}/event"
        self._api_token = api_token or None
        self._session_id = session_id or f"sdk-{uuid.uuid4().hex[:8]}"
        self._queue: "queue.Queue[Optional[bytes]]" = queue.Queue(maxsize=queue_max)
        self._closed = False
        self._lock = threading.Lock()
        self._worker = threading.Thread(
            target=self._run_worker,
            name="synaptic-disorder-emitter",
            daemon=True,
        )
        self._worker.start()

    # ------------------------------------------------------------------
    # Public surface
    # ------------------------------------------------------------------
    @property
    def session_id(self) -> str:
        return self._session_id

    @property
    def adapter_id(self) -> str:
        return self._adapter_id

    def register_adapter_id(self, adapter_id: str) -> None:
        """Change the adapter_id used on subsequent emits.

        Most callers just pass it to :func:`connect` and never need this. It
        exists so a long-running process can re-label itself if its role
        changes mid-session (e.g. embedded SDK that switches contexts).
        """
        self._adapter_id = adapter_id

    def emit_event(
        self,
        event_type: str,
        payload: Optional[Mapping[str, Any]] = None,
        *,
        region_hint: Optional[str] = None,
        session_id: Optional[str] = None,
    ) -> None:
        """Queue an event for delivery. Returns immediately."""
        validate_type(event_type)
        body = self._build(event_type, payload, region_hint, session_id)
        try:
            self._queue.put_nowait(body)
        except queue.Full:
            # Drop oldest by clearing one slot. Better to lose backlog than
            # block the calling agent.
            try:
                self._queue.get_nowait()
                self._queue.put_nowait(body)
            except (queue.Empty, queue.Full):
                pass

    # Short alias matching the JS API
    emit = emit_event

    def close(self, reason: str = "client_close") -> None:
        """Emit session_end and stop the worker."""
        if self._closed:
            return
        with self._lock:
            if self._closed:
                return
            self._closed = True
        self.emit_event("session_end", {"reason": reason})
        # Sentinel for the worker
        try:
            self._queue.put_nowait(None)
        except queue.Full:
            pass
        # Best-effort drain on close
        self._worker.join(timeout=1.5)

    # Context manager support
    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    # ------------------------------------------------------------------
    # Internals
    # ------------------------------------------------------------------
    def _build(
        self,
        event_type: str,
        payload: Optional[Mapping[str, Any]],
        region_hint: Optional[str],
        session_id: Optional[str],
    ) -> bytes:
        merged: dict[str, Any] = dict(payload or {})
        if region_hint and "region_hint" not in merged:
            merged["region_hint"] = region_hint
        if event_type == "session_start" and self._model and "model" not in merged:
            merged["model"] = self._model
        envelope = {
            "schema_version": "1.0",
            "type": event_type,
            "timestamp": _now_iso(),
            "adapter_id": self._adapter_id,
            "session_id": session_id or self._session_id,
            "payload": merged,
        }
        return json.dumps(envelope, separators=(",", ":")).encode("utf-8")

    def _run_worker(self) -> None:
        while True:
            try:
                body = self._queue.get()
            except Exception:
                return
            if body is None:
                return
            self._post(body)

    def _post(self, body: bytes) -> None:
        headers = {"Content-Type": "application/json"}
        if self._api_token:
            headers["Authorization"] = f"Bearer {self._api_token}"
        req = urllib.request.Request(
            self._url,
            data=body,
            headers=headers,
            method="POST",
        )
        try:
            # Short timeout — drop and move on if SD Core is unreachable.
            # Slightly longer than localhost-only since remote (CF Tunnel)
            # adds tunnel + TLS handshake overhead.
            urllib.request.urlopen(req, timeout=2.5).read()
        except (urllib.error.URLError, TimeoutError, OSError):
            # SD Core offline. We deliberately do not retry — backlog would
            # just grow until OOM and we're a fire-and-forget telemetry path.
            return
        except Exception:
            # Don't ever propagate from the worker; that would kill the thread
            # and break later emits.
            return


def connect(
    adapter_id: str,
    *,
    model: Optional[str] = None,
    host: Optional[str] = None,
    port: Optional[int] = None,
    url: Optional[str] = None,
    api_token: Optional[str] = None,
    session_id: Optional[str] = None,
    auto_session_start: bool = True,
) -> Client:
    """Create a :class:`Client` and (by default) emit ``session_start``.

    Environment variables override defaults:

    - ``SD_CORE_URL`` (e.g. ``https://cognito.example.com``) — preferred for
      remote operation; takes precedence over host+port when set.
    - ``SD_CORE_HOST`` (default ``127.0.0.1``)
    - ``SD_CORE_PORT`` (default ``9911``)
    - ``SD_API_TOKEN`` — Bearer token sent on every request when set.
    """
    url = url or os.environ.get("SD_CORE_URL") or None
    host = host or os.environ.get("SD_CORE_HOST") or "127.0.0.1"
    port = port or int(os.environ.get("SD_CORE_PORT") or 9911)
    api_token = api_token if api_token is not None else os.environ.get("SD_API_TOKEN")
    client = Client(
        adapter_id=adapter_id,
        model=model,
        host=host,
        port=port,
        url=url,
        api_token=api_token,
        session_id=session_id,
    )
    if auto_session_start:
        client.emit_event(
            "session_start",
            {"client": adapter_id, "model": model or "unknown"},
        )
    return client
