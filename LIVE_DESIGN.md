# Live transcription with fallback recording: design and contract

Goal for this build: transcribe a meeting **as it happens**, while a full-fidelity
recording is kept independently, so a crash, lag or poor live accuracy never costs the
meeting. Afterwards the full recording can be re-transcribed with a better model.

Target machine: Windows 11, Ryzen 5 7520U (4C/8T), 15 GB RAM, no NVIDIA GPU, so CPU only.

## Principles

1. **The recording never depends on transcription.** Audio is persisted twice before any
   ASR touches it: in the browser (IndexedDB) and on the server (append-only file).
   ASR is a *consumer* of the server-side file, never a gatekeeper.
2. **Everything is resumable.** Browser tab crash, network drop, server restart, ASR
   worker crash. Each one has a recovery path that loses at most about 1 s of audio, and
   usually nothing.
3. **Live text is a draft.** It is stored separately (`live_transcript`), shown
   immediately, and can be superseded by a post-meeting high-quality pass without losing it.
4. **No new transport stack.** Plain HTTP POSTs for audio chunks, plus polling for
   transcript state. No WebSockets: no Vite proxy changes, no token-in-URL auth, and
   nothing to drop silently. SSE is not used for live text, because the existing
   broadcaster drops events.

## Data flow

```
 Browser                                          Server (Go)                          Worker (Python, persistent)
 -------                                          -----------                          ---------------------------
 mic (+ system audio via getDisplayMedia)
   -> AudioContext mix -> MediaStreamDestination
   -> MediaRecorder(audio/webm;codecs=opus, 1 s timeslice)
        each chunk (seq = 0,1,2,...):
        1) IndexedDB put [sessionId, seq]  (durable locally)
        2) POST /api/v1/live/sessions/:id/chunks?seq=N ----> append to recording.webm (+fsync)
                                                            append (seq,offset,len) to chunks.idx
                                                            |
                                                            tail-reader over recording.webm
                                                            -> ffmpeg (webm -> s16le 16 kHz mono)
                                                            -> PCM ------------------------------> stdin
                                                                                                    sherpa-onnx streaming ASR
                                                            <- JSON lines (partial/final/progress) <- stdout
                                                            -> live_segments table (+ partial in memory)
 poll GET /api/v1/live/sessions/:id/state?after=K  <------
   (every 1 s; renders finals + current partial)
 Stop -> drain upload queue -> POST .../stop ---------> close tail -> ffmpeg/worker flush -> last finals
                                                            remux recording.webm -> recording.final.webm (seekable)
                                                            materialize segments -> job.transcript + job.live_transcript
                                                            status=completed, live_status=finalized
                                                            optional: enqueue high-quality re-transcription
```

## Storage layout (server)

```
data/uploads/live/<job_id>/
  recording.webm        append-only raw MediaRecorder stream (THE fallback; never modified)
  chunks.idx            one line per accepted chunk: "<seq> <byte_offset> <byte_len>\n"
  recording.final.webm  ffmpeg -c copy remux at finalize (has duration and cues, seekable); job.audio_path points here
  worker.log            stderr of the ASR worker and ffmpeg, appended across restarts
```

Each live session is a normal `transcription_jobs` row. The library, detail page, notes,
chat, summaries and delete all keep working.

## Database changes (`internal/models`)

New columns on `TranscriptionJob`. All are nullable or defaulted so AutoMigrate can add them:

| field | gorm | meaning |
|---|---|---|
| `Source string` | `type:varchar(20);default:'upload';index` | `upload` or `live` |
| `LiveStatus string` | `type:varchar(20);default:'none';index` | `none`, `recording`, `interrupted`, `finalizing`, `finalized`, `failed` |
| `LiveTranscript *string` | `type:text` | TranscriptResult JSON of the live pass. Never cleared by re-transcription |
| `TranscriptSource string` | `type:varchar(20);default:'batch'` | which pass currently fills `Transcript`: `live` or `batch` |
| `RecordingPath *string` | `type:text` | path to the raw `recording.webm` |

While recording, `Status` stays `uploaded`. The queue, zombie reset and pending recovery
then ignore the row, and the normal "Transcribe" action can later run the high-quality
pass on it. At finalize, the status becomes `completed`, with the live transcript in
`Transcript` and `TranscriptSource='live'`.

New table `LiveSegment` (`internal/models/live.go`):
`ID uint pk; TranscriptionJobID varchar(36) not null, uniqueIndex(job,seq); Seq int; StartSec float64; EndSec float64; Text text; WordsJSON *string (optional [{word,start,end}]); CreatedAt`.
Times are absolute session seconds.

`GetTranscript`: when `Status != completed`, a `LiveTranscript` exists, and a
re-transcription is pending, processing or failed, return the live transcript instead of
`available:false`. Then a failed high-quality pass never leaves the meeting with nothing.

## HTTP API (all under `/api/v1/live`, `AuthMiddleware`, no gzip)

### `POST /sessions`
Request JSON: `{ "title": "Weekly sync", "mime_type": "audio/webm;codecs=opus", "auto_retranscribe": false }`
Response `201`: `{ "id": "<job_id>", "chunk_ms": 1000 }`
Creates the job row (`source=live`, `live_status=recording`, `status=uploaded`,
`audio_path` = `data/uploads/live/<id>/recording.webm`), creates the directory, and starts
the ASR pipeline (it waits for bytes).

### `POST /sessions/:id/chunks?seq=N`
Body: raw chunk bytes (`Content-Type: application/octet-stream`; up to 8 MiB).
- `seq == next_seq`: append, fsync (at least every 5 chunks and on stop), index, then `200 {"ack": N, "next_seq": N+1}`
- `seq < next_seq`: duplicate (a client retry). Do not append. `200 {"ack": N, "next_seq": next_seq, "duplicate": true}`
- `seq > next_seq`: gap. `409 {"error": "gap", "next_seq": next_seq}`. The client resends from `next_seq` out of IndexedDB.
- Session `finalizing`, `finalized` or `failed`: `409 {"error": "closed", "live_status": ...}`
- Session `interrupted` (server restarted mid-meeting): **reopen**. Reload `next_seq` from
  `chunks.idx`, truncate `recording.webm` to the last indexed end offset, restart the ASR
  pipeline (resuming after the last final segment), set `live_status=recording`, then
  process the chunk normally.

### `GET /sessions/:id/state?after=K`
Response `200`:
```json
{
  "id": "...", "title": "...", "live_status": "recording", "status": "uploaded",
  "next_seq": 123, "bytes": 1534000,
  "received_audio_sec": 121.0,          // audio decoded by ffmpeg so far
  "asr": { "state": "running", "processed_sec": 119.4, "lag_sec": 1.6, "engine": "nemotron-en-1120ms",
           "restarts": 0, "error": "" },  // state: starting|running|restarting|stopped|failed|unavailable
  "segments": [ { "seq": 7, "start": 30.2, "end": 34.9, "text": "..." } ],   // only seq > K
  "partial": { "text": "and then we", "start": 118.0 },                       // may be null
  "auto_retranscribe": false
}
```
The client polls about once per second. `after=-1` returns all segments.

### `POST /sessions/:id/stop`
Request JSON: `{ "last_seq": 122 }`, the highest seq the client recorded.
- If `next_seq <= last_seq`: `409 {"error": "missing_chunks", "next_seq": ...}`. The client uploads the rest, then retries.
- Otherwise: `202 {"live_status": "finalizing"}`, and finalize runs asynchronously:
  1. Close the tail reader (EOF) so ffmpeg flushes and the worker flushes its last finals
     and exits (wait up to 120 s).
  2. `ffmpeg -i recording.webm -c copy recording.final.webm` (if that fails, keep using
     recording.webm). Set `audio_path` to the result.
  3. Materialize segments into TranscriptResult JSON:
     `{text, language:"en", segments:[{start,end,text}], word_segments:[{word,start,end}], model_used}`.
     Word timings come from the worker when available. Otherwise distribute each
     segment's span evenly over its words: the detail view needs `word_segments`.
  4. Write `transcript` and `live_transcript`, then set `transcript_source=live`,
     `status=completed`, `live_status=finalized`.
  5. If `auto_retranscribe`, enqueue the normal pipeline on the job with the user's
     default profile (same path as `POST /transcription/:id/start`).

  On any error the status is `live_status=failed`, but the recording files always remain.

### `POST /sessions/:id/finalize`
Finalizes an `interrupted` session without the client, for example after a crash where the
browser never came back. Same steps as stop, using whatever chunks arrived.

### `GET /sessions?status=recording,interrupted`
Lists the user's live sessions that are not yet finalized, for the recovery UI.

## Server-side robustness

- **ASR supervisor**: if ffmpeg or the worker exits while the session is `recording`,
  restart both with backoff (1, 2, 5, 10, then 30 s). On restart, ffmpeg re-reads
  `recording.webm` from byte 0 (opus decodes far faster than real time), and the Go side
  discards PCM before `resume_at` (the end of the last final segment). Then there is no
  duplicate text and no header problem: WebM chunks after the first are not decodable on
  their own. Recording is never paused by ASR trouble.
- **Startup recovery**: sessions found with `live_status=recording` become `interrupted`
  and wait for the client to resume. A janitor finalizes `interrupted` sessions that
  receive no chunk for `LIVE_ABANDON_MINUTES` (default 30).
- **CPU budget**: the worker runs with `LIVE_THREADS` (default 4), and the batch queue
  stays at one worker (`QUEUE_WORKERS=1`).
- **Logging**: the chunk and state endpoints are excluded from per-request Info logs.

## Worker protocol (`internal/transcription/adapters/py/live/live_worker.py`)

Run as `uv run --project <WHISPERX_ENV>/live python live_worker.py --engine <e> --models-dir <d> --threads 4`.
- **stdin**: raw PCM, s16le, 16 kHz, mono, written by Go in whatever sizes. EOF means flush and exit.
- **stdout**: one JSON object per line (UTF-8, flushed per line):
  - `{"type":"ready","engine":"nemotron-en-1120ms","sample_rate":16000}` once the model is loaded
  - `{"type":"partial","text":"...","start":12.3}`, at most every 250 ms, and only when the text changes
  - `{"type":"final","text":"...","start":10.1,"end":14.8,"words":[{"word":"Hello","start":10.1,"end":10.4}]}`
  - `{"type":"progress","processed_sec":118.2}` about every second
  - `{"type":"eof"}` after flushing on stdin EOF, then exit 0
  - `{"type":"error","message":"..."}` on a fatal error, then exit non-zero

  Times are seconds since the first sample this worker received. Go adds the
  `resume_at` offset.
- **stderr**: free-form logs.
- **Engines**:
  - `nemotron-en-1120ms` (default) and `nemotron-en-560ms`: sherpa-onnx `OnlineRecognizer`, true streaming, endpoint detection
  - `parakeet-v3-vad`: Silero VAD plus offline Parakeet TDT v3 per utterance. 25 European languages, no partials.
- Models come from `--models-dir` (default `data/live-models`), with archive names as
  published by k2-fsa. If missing, the worker downloads and extracts them (tar.bz2), and
  logs progress to stderr.

## Browser side (`web/frontend/src/features/live/`)

- `/live` route, linked from the header. The recorder lives in a provider mounted above
  the routes, so navigating away never stops it, and a "Live" indicator shows anywhere in the app.
- Sources: Microphone; Microphone plus computer audio; Computer audio only. Computer
  audio comes from `getDisplayMedia`. On Windows Chrome/Edge, pick "Entire screen" and
  tick "Share system audio" for desktop Zoom/Teams, or pick a tab and tick "Share tab audio".
- MediaRecorder `audio/webm;codecs=opus` at 128 kbps with a 1 s timeslice. Each chunk
  is first written to IndexedDB (`scriberr-live` DB: `sessions` and `chunks` stores),
  then uploaded by a sequential uploader with retry and exponential backoff (cap 10 s)
  that handles `409 gap` by resending from `next_seq`.
- `navigator.storage.persist()` and a screen Wake Lock while recording; a `beforeunload` warning.
- Live transcript panel: finals with timestamps, the partial in a muted style,
  auto-scroll (paused when the user scrolls up). Status bar: elapsed time, local save
  state, uploaded/recorded chunks, ASR state and lag.
- Stop: `recorder.stop()`, drain the uploader (with progress; on failure the meeting
  stays safe locally), `POST stop`, poll state until `finalized`, then link to `/audio/:id`.
- Recovery: on load, IndexedDB sessions that are not finalized show "Resume upload",
  "Download recording (.webm)" or "Discard". Local chunks are deleted only after the
  server reports `finalized`.

## Configuration (`.env`)

| key | default | |
|---|---|---|
| `LIVE_ENABLED` | `true` | set to `false` to remove the live routes |
| `LIVE_ENGINE` | `nemotron-en-1120ms` | worker engine |
| `LIVE_THREADS` | `4` | ONNX threads for the worker |
| `LIVE_MODELS_DIR` | `data/live-models` | model archives |
| `LIVE_ABANDON_MINUTES` | `30` | auto-finalize interrupted sessions |

## Out of scope for the first cut

Live speaker diarization; per-speaker tracks (mic vs system as separate channels);
native WASAPI capture outside the browser; translating live.
