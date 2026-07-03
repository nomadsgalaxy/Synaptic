# synaptic (Python SDK)

Tiny Python client for emitting Synaptic activity events. Pure stdlib — **no runtime dependencies**.

## Install

For now, install directly from this folder:

```bash
pip install -e bridge/sdk/python
```

Once published to PyPI:

```bash
pip install synaptic
```

## Use

```python
import synaptic as sd

with sd.connect(adapter_id="my-agent", model="claude-opus-4-7") as client:
    client.emit("prompt_received", {"text": "hi", "char_count": 2}, region_hint="wernicke_area")
    client.emit("tool_call", {"tool_name": "bash"}, region_hint="motor_cortex")
    client.emit("memory_added", {"memory_id": "m-1", "text": "User said hi"})
# session_end is emitted on context exit
```

## API

### `connect(adapter_id, *, model=None, host=None, port=None, url=None, api_token=None, session_id=None, auto_session_start=True) → Client`

Returns a `Client`. By default, immediately emits `session_start` with `payload.client = adapter_id` and `payload.model = model`.

Environment overrides:

- `SD_CORE_URL` — full URL (e.g. `https://cognito.example.com`). Preferred for remote operation; takes precedence over host+port.
- `SD_CORE_HOST` (default `127.0.0.1`)
- `SD_CORE_PORT` (default `9911`)
- `SD_API_TOKEN` — Bearer token sent on every request when set. Required when SD Core was started with `SD_API_TOKEN`.

Example for a remote SD Core behind Cloudflare Tunnel:

```python
import os
os.environ['SD_CORE_URL']  = 'https://cognito.example.com'
os.environ['SD_API_TOKEN'] = '<the same token SD Core was started with>'

import synaptic as sd
with sd.connect('my-agent', model='claude-opus-4-7') as client:
    client.emit('prompt_received', {'text': 'hi'})
```

See [`docs/REMOTE.md`](../../../docs/REMOTE.md) for the full deployment guide.

### `Client.emit(event_type, payload=None, *, region_hint=None, session_id=None) → None`

Validates `event_type` against the v1.0 enum (raises `SchemaError` if unknown). Queues the event for delivery on a daemon thread. Returns immediately.

`region_hint` is folded into the payload if not already there. `session_id` overrides the auto-generated one for this single emit.

`emit_event` is the long-form alias.

### `Client.register_adapter_id(adapter_id) → None`

Change the `adapter_id` used on subsequent emits. Useful for long-lived processes that switch roles mid-session.

### `Client.close(reason='client_close') → None`

Emits `session_end` with the given reason and stops the worker thread. Safe to call multiple times. Also runs automatically when used as a context manager.

### `Client.session_id`, `Client.adapter_id`

Read-only access.

## Examples

- [`examples/hello_brain.py`](./examples/hello_brain.py) — bare-minimum 5-event script
- [`examples/wrap_function.py`](./examples/wrap_function.py) — `@trace_tool` decorator

## Verifying

```bash
# 1. SD Core up?
curl http://localhost:9911/healthz

# 2. Run an example
python -m synaptic._client  # nothing happens — that's just the module
python bridge/sdk/python/examples/hello_brain.py

# 3. SD Core saw the events?
curl 'http://localhost:9911/events?adapter_id=hello-brain' | python -m json.tool
```

## Dropping rules

- **Schema-invalid types** raise `SchemaError` immediately (you'll see the traceback).
- **Network failure / SD Core offline** is swallowed silently — the client is a telemetry path; we never want to break the host program.
- **Emit faster than the network can flush** drops the oldest queued event. Default queue size is 1024.

## Packaging

`pyproject.toml` is set up for `pip install -e .` and standard `python -m build` workflows.

## License

Pending — Open Community License.
