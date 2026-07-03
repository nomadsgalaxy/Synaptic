import json
from datetime import datetime, timezone
with open(r'C:\Users\Anthony\AppData\Local\Temp\_research.json', encoding='utf-8') as f:
    d = json.load(f)
items = d.get('entries', d.get('items', d.get('research', d))) if isinstance(d, dict) else d
if isinstance(items, dict):
    items = list(items.values())
if not isinstance(items, list):
    items = []
print(f'total research entries returned: {len(items)}')
now = datetime.now(timezone.utc)
expired = []
live = []
for r in items:
    exp_raw = r.get('expires_at', '') or ''
    try:
        exp = datetime.fromisoformat(exp_raw.replace('Z', '+00:00'))
        (expired if exp < now else live).append(r)
    except Exception:
        pass
print(f'  live (not expired):    {len(live)}')
print(f'  expired (in the past): {len(expired)}')
if expired:
    print('\nsample expired entries:')
    for r in expired[:5]:
        q = (r.get('query') or r.get('topic') or '')[:60]
        fa = (r.get('fetched_at') or '')[:10]
        ea = (r.get('expires_at') or '')[:10]
        print(f'  fetched={fa} expires={ea} query={q!r}')
