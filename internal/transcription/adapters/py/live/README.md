# Live ASR worker

`live_worker.py` is the persistent speech-to-text process behind Scriberr live sessions
(see `LIVE_DESIGN.md` at the repo root). The Go server starts one worker per live
session, pipes 16 kHz mono PCM into it, and reads transcript events back.

```
uv run --project <WHISPERX_ENV>/live python live_worker.py \
    --engine nemotron-en-1120ms --models-dir data/live-models --threads 4
```

| flag | default | |
|---|---|---|
| `--engine` | `nemotron-en-1120ms` | `nemotron-en-1120ms`, `nemotron-en-560ms` or `parakeet-v3-vad` |
| `--models-dir` | `data/live-models` | where the k2-fsa model archives live |
| `--threads` | `4` | ONNX threads for the recognizer |
| `--partials` / `--no-partials` | on | partial text; for `parakeet-v3-vad` each partial re-decodes the utterance so far |
| `--log-level` | `info` | stderr logging: `debug` also logs every final |

## Protocol

- **stdin**: raw PCM, s16le, 16 kHz, mono, in writes of any size. EOF means flush and exit.
- **stdout**: one JSON object per line, UTF-8, flushed per line:
  - `{"type":"ready","engine":"nemotron-en-1120ms","sample_rate":16000}` once the model is loaded
  - `{"type":"partial","text":"...","start":12.3}` at most every 250 ms, only when the text changes
  - `{"type":"final","text":"...","start":10.1,"end":14.8,"words":[{"word":"Hello","start":10.1,"end":10.4}]}`
  - `{"type":"progress","processed_sec":118.2}` about once per second
  - `{"type":"eof"}` after the flush on stdin EOF, then exit code 0
  - `{"type":"error","message":"..."}` on a fatal error, then exit code 1
- **stderr**: free-form logs (model loading, download progress, compute RTF every minute).

Times are seconds since the first sample this worker received; the Go side adds its
`resume_at` offset. `words` is always present and may be empty.

## Engines

Measured on the Ryzen 5 7520U with 4 threads, feeding the AMI clip at real-time speed
(`tests/test_live_worker.py --realtime`); "final delay" is from the end of the last word
to the final event arriving.

| engine | how | final delay | CPU (compute / audio) |
|---|---|---|---|
| `nemotron-en-1120ms` | sherpa-onnx `OnlineRecognizer`, NVIDIA Nemotron Speech Streaming EN 0.6B int8, 1.12 s chunks; partials about every 1.1 s | 1.2–1.8 s | 0.41 |
| `nemotron-en-560ms` | same model exported with 0.56 s chunks | 1.0–1.2 s | 0.63 |
| `parakeet-v3-vad` | Silero VAD cuts utterances (0.5 s min silence, 15 s max), offline Parakeet TDT v3 int8 per utterance; 25 European languages | ~0.5 s + 0.15 × utterance length after the utterance closes | 0.37 (0.18 with `--no-partials`) |

Model load takes 4–6 s; memory is about 1 GB and stayed flat over a 12-minute run.

Nemotron endpointing: a segment ends after 0.8 s of silence following speech, after
2.4 s of silence with no speech, or at 20 s. The worker prepends 0.3 s of silence to the
stream and appends 1.5 s at EOF, so the first and last words are not clipped. Word
timings come from the token timestamps (a token starting with `▁`, or a space, starts a
word); words that share an encoder frame are spread evenly up to the next word. Parakeet
TDT also gives token durations.

`parakeet-v3-vad`: Silero's `max_speech_duration` is only a soft limit, and on meeting
audio it let a 19 s utterance through, so the worker also cuts an utterance at its
quietest 30 ms within its 10–15 s window and finalizes it. Partials re-decode the
utterance in progress at most every max(1 s, 4 × last decode time), so they use at most
about a quarter of the decode CPU.

## Models

The worker looks for these under `--models-dir`, as published at
`https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/`:

- `sherpa-onnx-nemotron-speech-streaming-en-0.6b-1120ms-int8-2026-04-25/`
- `sherpa-onnx-nemotron-speech-streaming-en-0.6b-560ms-int8-2026-04-25/`
- `sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/` plus `silero_vad.onnx`

A missing model is downloaded (`<archive>.tar.bz2`, about 450–650 MB) and extracted on
first use, with progress on stderr. Both steps use temporary names and rename into place,
so an interrupted download is never mistaken for a model. `ready` is only sent after
that, so the first start can take minutes.

## Testing

```
uv run --project data/whisperx-env/live python internal/transcription/adapters/py/live/tests/test_live_worker.py --engine all --realtime
```

The test pipes `tests/data/AMI-Corpus-IB4002.Mix-Headset-clip.wav` through ffmpeg into
the worker (as the server does), then repeats it with odd-sized writes, empty stdin and
pure silence, and checks every protocol rule. `--realtime` feeds audio at real-time speed
and prints when each final arrives relative to the end of its speech. `--dump DIR` keeps
the raw JSON lines and stderr of every run.

PowerShell pipes mangle binary data between native commands, so for a manual run pipe
through `cmd /c`:

```
cmd /c "ffmpeg -hide_banner -loglevel error -i tests\data\AMI-Corpus-IB4002.Mix-Headset-clip.wav -f s16le -ac 1 -ar 16000 pipe:1 | uv run --project data\whisperx-env\live python internal\transcription\adapters\py\live\live_worker.py --models-dir data\live-models"
```
