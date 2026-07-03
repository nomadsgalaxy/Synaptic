# Install scripts (D4)

Cross-platform helpers for running Synaptic. The Docker stack itself is the actual installer; these are thin convenience wrappers.

## What ships now

| File | Platform | What it does |
|---|---|---|
| `start.ps1` | Windows | `docker compose up -d` + waits for dashboard + opens browser |
| `start.sh` | macOS / Linux | Same, with `xdg-open` / `open` |
| `../../start.bat` | Windows | Double-clickable launcher that calls `start.ps1` |

Both work from the repo root: clone, then run `start.ps1` or `./start.sh`.

## Real platform installers — pending (D4)

The original D4 plan calls for native installers per OS:

- **Windows `.exe`** — Inno Setup (`.iss` script). Bundles the repo + a small C-side launcher that ensures Docker Desktop is running before invoking `start.ps1`.
- **macOS `.dmg`** — `create-dmg`. App bundle with an embedded `start.sh` and an icon that lives in `/Applications/Synaptic.app`.
- **Linux `.deb` / `.rpm`** — `fpm`. Installs the repo to `/opt/synaptic-disorder/` and a `synaptic-disorder` shim into `/usr/local/bin/`.
- **Portable ZIP** — produced by `tools/build_release.py --zip`. Already shippable.

Each native installer needs to be built ON its target platform (or with a cross-builder), so they're sketched here as TODO files rather than committed working configs:

- `inno-setup.iss.todo` — Inno Setup script outline (run with ISCC.exe on Windows)
- `create-dmg.sh.todo` — DMG build script outline (run on macOS)
- `fpm-spec.sh.todo` — fpm command outline (run on a Linux box with fpm installed)

## Portable zip (no installer)

```bash
python tools/build_release.py --zip
# produces release/synaptic-disorder.zip
```

Unzip anywhere, then run `start.ps1` (Windows) or `start.sh` (macOS/Linux).
