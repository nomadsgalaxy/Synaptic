#!/usr/bin/env python3
# One-shot end-to-end test for the Bundle S hook-synthesis engine.
# Enables the feature, drives a synthetic error cluster + session_end
# through /event, and confirms a hook-synthesized memory lands in the bank.
import json, urllib.request, urllib.error, time, subprocess

TOKEN = subprocess.check_output(
    "grep ^SD_API_TOKEN /home/debian/synaptic/.env | cut -d= -f2",
    shell=True).decode().strip()
BASE = "http://localhost:9911"


def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(
        BASE + path, data=data, method=method,
        headers={"Authorization": "Bearer " + TOKEN,
                 "Content-Type": "application/json",
                 "X-Adapter-Id": "deploy-test"})
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)


print("=== 1. Enable settings ===")
for k, v in [("hook_synthesis_enabled", "1"),
             ("hook_synthesis_error_threshold", "2")]:
    s, b = req("PUT", "/settings/" + k, {"value": v})
    print("  PUT {}={} -> {} {}".format(k, v, s, b[:80]))

print("=== 2. Confirm via GET ===")
for k in ["hook_synthesis_enabled", "hook_synthesis_error_threshold"]:
    s, b = req("GET", "/settings/" + k)
    print("  {} -> {} {}".format(k, s, b[:80]))

print("=== 3. Post error cluster + session_end (session=hooktest-deploy-001) ===")
SID = "hooktest-deploy-001"


def ev(typ, payload):
    return {"schema_version": "1.0", "type": typ,
            "timestamp": "2026-05-31T00:45:00.000Z",
            "adapter_id": "deploy-test", "session_id": SID,
            "payload": payload}


for i in range(3):
    s, b = req("POST", "/event", ev("error", {
        "where": "tool:Bash",
        "error_message": "npm: command not found",
        "tool_call_id": "t{}".format(i)}))
    print("  error[{}] -> {}".format(i, s))
s, b = req("POST", "/event", ev("session_end", {"reason": "deploy-test-complete"}))
print("  session_end -> {}".format(s))

print("=== 4. Wait 30s for async synthesis (Tier 1 gpt-4o-mini) ===")
time.sleep(30)

print("=== 5. Query bank for hook-synthesized memories (trigger: tags) ===")
s, b = req("GET", "/bank/memories?limit=40")
d = json.loads(b)
mems = d.get("memories", d if isinstance(d, list) else [])
hits = [m for m in mems if any(t.startswith("trigger:") for t in m.get("tags", []))]
print("  scanned {} recent memories; {} carry a trigger: tag".format(len(mems), len(hits)))
for m in hits[:6]:
    trig = [t for t in m.get("tags", []) if t.startswith("trigger:")]
    mid = m.get("id")
    created = (m.get("created_at", "") or "")[:19]
    src = m.get("source")
    txt = (m.get("text", "") or "")[:280]
    print("  ---")
    print("  id={} created={} {} source={}".format(mid, created, trig, src))
    print("  text: {}".format(txt))
if not hits:
    print("  !! NO hook-synthesized memories found — checking core logs for errors")
