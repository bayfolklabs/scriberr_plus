# Scriberr on this Windows laptop: setup, run, tweak

This checkout is Scriberr (upstream `rishikanthc/Scriberr`), set up to build and run
natively on Windows 11 with no Docker and no NVIDIA GPU. It also has a new **live meetings**
feature: transcribe while the meeting happens, with a full recording kept as a fallback.
For the design and API contract, see [LIVE_DESIGN.md](LIVE_DESIGN.md).

## What's installed

| Tool | How | Used for |
|---|---|---|
| Go 1.27 | winget `GoLang.Go` | backend (`go.mod` asks for 1.24+) |
| Node 24 LTS | winget `OpenJS.NodeJS.LTS` | frontend build / Vite dev server |
| Git | winget `Git.Git` | WhisperX clone, version control |
| FFmpeg 9 (full) | winget `Gyan.FFmpeg` | audio conversion, live decoding |
| uv 0.13 | winget `astral-sh.uv` | Python model environments |
| Python 3.11 | `uv python install 3.11` (in `C:\Users\me\.cache\uv\python`) | model workers |

Open a **new** terminal after installs so PATH is current. The scripts below refresh
PATH themselves.

## Daily commands (PowerShell, from `C:\Apps\Scriberr`)

```bash
powershell -ExecutionPolicy Bypass -File scripts\build.ps1
```
This builds the React UI, embeds it, and writes `scriberr.exe`. Add `-SkipFrontend` for
backend-only rebuilds.

```bash
.\scriberr.exe
```
Starts the server at http://localhost:8080 (bound to 127.0.0.1 only). Run it from the
repo root: the `data\` paths are relative.

```bash
powershell -ExecutionPolicy Bypass -File scripts\dev.ps1
```
Dev loop: backend on :8080 plus the Vite dev server with hot reload on
http://localhost:5173. Ctrl+C stops both.

```bash
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1
```
End-to-end check: log in, upload `tests\data` audio, transcribe with Whisper `base.en`
int8, then print the transcript. The first run creates a local test account (`devtester`)
and keeps its credentials in `data\dev-credentials.json`. You can sign in to the UI with it,
or register your own account.

```bash
powershell -ExecutionPolicy Bypass -File scripts\live-simulate.ps1 -Repeat 3
```
Streams audio through a real live session in real time, the way the browser does, then
prints live text, stops and finalizes. Options: `-Speed 2` (faster than real time),
`-UntilSeq 25 -NoStop` and `-SessionId <id> -FromSeq 25` (crash/resume testing),
`-AutoRetranscribe`.

Tests: `go test ./internal/... ./tests/...` (one LLM suite test is flaky when run with the
others; it passes alone). Frontend: `npm run lint`, `npx tsc -b` in `web\frontend`. Worker:
`uv run --project data\whisperx-env\live python internal\transcription\adapters\py\live\tests\test_live_worker.py --engine all`.

## Using live meetings

1. Open http://localhost:8080 and click **+**, then **Live Meeting** (or go to `/live`).
2. Choose the source:
   - **Microphone**: in-person meetings.
   - **Microphone + computer audio**: online calls. Chrome/Edge will ask what to share.
     - Desktop Zoom/Teams: **Entire screen** and **Share system audio**.
     - A meeting in a browser tab: that tab and **Share tab audio**.
   - **Computer audio only**: webinars.
3. Optional: tick **Re-transcribe full recording after the meeting**. It runs your default
   profile on the complete recording afterwards. Set a profile up in Settings first;
   Whisper `small`/`medium` with `int8` works on this CPU.
4. **Start**. Live text appears about 1–2 s behind speech. The status bar shows local save,
   upload and ASR state. You can navigate around the app while it records.
5. **Stop**. The rest is uploaded and the session finalized (about 1 s), and it opens like
   any other transcript: playback, word highlighting, notes, chat, summary, re-transcribe.

What survives what:

| Failure | What happens |
|---|---|
| ASR worker crashes or lags | Recording continues untouched. The worker restarts (backoff) and resumes after the last final segment. |
| Network or server down | Chunks queue in the browser (IndexedDB) and upload when it's back. |
| Server crash or restart mid-meeting | The session is marked `interrupted`. The next chunk reopens it and live text resumes. |
| Browser tab or crash | Chunks are already in IndexedDB. `/live` shows **Resume upload / Download recording / Discard**. |
| Never came back | The server auto-finalizes after `LIVE_ABANDON_MINUTES` (30) with what it has. |

On the server, each meeting is `data\uploads\live\<id>\`:
- `recording.webm`: raw, never modified.
- `recording.final.webm`: seekable copy used for playback.
- `chunks.idx`: the chunk index.
- `worker.log`: ASR worker output.

Live text is a draft. For the best transcript, re-transcribe the finished job with a bigger
model. The live draft is kept, and shown again if the re-transcription fails.

## Configuration (`.env`, gitignored; template in `.env.example`)

| Key | Value here | Why |
|---|---|---|
| `HOST` | `127.0.0.1` | not exposed on the LAN |
| `APP_ENV` / `SECURE_COOKIES` | `development` / `false` | plain-http localhost logins |
| `SCRIBERR_ADAPTERS` | `whisperx,openai_whisper` | **new**: only these model envs are set up at startup (all 7 by default upstream) |
| `PYTORCH_CUDA_VERSION` | `cpu` | CPU torch wheels instead of multi-GB CUDA ones |
| `UV_PYTHON`, `UV_PYTHON_INSTALL_DIR`, `UV_CACHE_DIR` | `3.11`, `C:/Users/me/.cache/uv/...` | keeps uv out of `%APPDATA%` (see gotchas) |
| `PYTHONUTF8`, `PYTHONIOENCODING` | `1`, `utf-8` | model scripts print UTF-8; the Windows console is cp1252 |
| `QUEUE_WORKERS` | `1` | one batch job at a time on 4 cores |
| `LIVE_ENGINE` | default `nemotron-en-1120ms` | or `nemotron-en-560ms` (lower latency, more CPU), `parakeet-v3-vad` (25 European languages) |
| `LIVE_THREADS` | default `4` | ONNX threads for the live worker |
| `LIVE_MODELS_DIR` | default `data/live-models` | models are auto-downloaded there if missing |
| `LIVE_ABANDON_MINUTES` | default `30` | auto-finalize interrupted sessions |
| `LIVE_ENABLED` | default `true` | `false` removes the live routes |

To try other batch models, add them to `SCRIBERR_ADAPTERS`:
- `parakeet` and `sortformer` pull NVIDIA NeMo. This is heavy, unofficial on Windows, and
  may need the VS Build Tools.
- `pyannote` diarization needs `HF_TOKEN`.
- `canary` (8 GB+ RAM) and `voxtral` (about 19 GB RAM) won't run well here.

## Live engines on this CPU (Ryzen 5 7520U, 4 threads, real-time pacing)

| Engine | First text | Final text after speech | CPU (fraction of real time) |
|---|---|---|---|
| `nemotron-en-1120ms` (default) | ~1.5 s | ~1.2–1.8 s | ~0.3–0.4 |
| `nemotron-en-560ms` | ~1.3 s | ~1.0–1.2 s | ~0.6 |
| `parakeet-v3-vad` | ~1.0 s (partials) | ~2 s after each utterance ends | ~0.2–0.4 |

These were measured on the AMI meeting clip while other work was running, so a quiet machine
does a little better. Memory is about 1 GB for the worker. The batch Whisper `base.en` int8
pass on the same clip works too (first run includes model downloads).

## Gotchas found while setting this up

- **AppData redirection in the Claude desktop app.** Commands run from a terminal inside the
  Claude app get `%APPDATA%` writes redirected into a private package folder. That broke uv's
  Python install links. The `.env` points uv at `C:\Users\me\.cache\uv` to avoid it.
- **First start is slow.** It downloads the WhisperX environment (CPU torch about 2.7 GB
  unpacked; Defender scanning slows it), Whisper models, and the wav2vec2 alignment model
  (360 MB). Later starts take about 2 s.
- The macOS binaries `server`/`main` and `Scriberr-main.zip` in the repo root are from the
  original archive. They're ignored by git and can't run here.
- `go test ./tests/...` used to create `C:\root\...`; that's fixed in `tests/database_test.go`.

## Where to tweak

| Area | Files |
|---|---|
| Live session server (chunks, pipeline, finalize, recovery) | `internal/live/*.go`, `internal/api/live_handlers.go` |
| Live ASR worker (engines, endpointing, word timings) | `internal/transcription/adapters/py/live/live_worker.py` (copied into `data\whisperx-env\live\` at every server start) |
| Live UI | `web/frontend/src/features/live/*` (`liveController.ts` is the state machine) |
| Batch model adapters | `internal/transcription/adapters/*.go`, plus their `py/` scripts |
| Startup / adapter allowlist | `cmd/server/main.go`, `internal/config/config.go` |

Git history: `ca41087` is the untouched upstream baseline, and each change after it is a
separate commit (`git log --oneline`).
