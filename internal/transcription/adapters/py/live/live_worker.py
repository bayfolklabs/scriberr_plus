#!/usr/bin/env python3
"""
Persistent live (streaming) ASR worker for Scriberr live sessions.

One process serves one live session. It reads raw PCM from stdin and writes one JSON
event per line to stdout (see LIVE_DESIGN.md, "Worker protocol"):

  stdin   s16le, 16 kHz, mono, in writes of any size. EOF means flush and exit.
  stdout  {"type":"ready","engine":"nemotron-en-1120ms","sample_rate":16000}
          {"type":"partial","text":"...","start":12.3}
          {"type":"final","text":"...","start":10.1,"end":14.8,"words":[{"word":"Hello","start":10.1,"end":10.4}]}
          {"type":"progress","processed_sec":118.2}
          {"type":"eof"}
          {"type":"error","message":"..."}
  stderr  free-form logs.

Times are seconds since the first sample this worker received.

Engines:
  nemotron-en-1120ms (default), nemotron-en-560ms
      sherpa-onnx OnlineRecognizer: true streaming with endpoint detection.
  parakeet-v3-vad
      Silero VAD plus offline Parakeet TDT v3 per utterance (25 European languages).

Models live in --models-dir under the archive names published by k2-fsa. Missing
models are downloaded and extracted on first use.
"""

import argparse
import glob
import json
import logging
import os
import shutil
import sys
import tarfile
import time
import urllib.request

import numpy as np

SAMPLE_RATE = 16000
BLOCK_SAMPLES = SAMPLE_RATE // 10  # feed the recognizer in 100 ms blocks
BLOCK_BYTES = BLOCK_SAMPLES * 2
READ_SIZE = 64 * 1024

PREROLL_SEC = 0.3  # silence before the first sample, so the first words are not clipped
TAIL_PAD_SEC = 1.5  # silence after EOF, so the last words get decoded
PARTIAL_INTERVAL_SEC = 0.25
PROGRESS_INTERVAL_SEC = 1.0
STATS_INTERVAL_SEC = 60.0

# Word end when a token has no duration: the last token's time plus this, capped by
# the start of the next word.
WORD_TAIL_SEC = 0.3
SP_SPACE = chr(0x2581)  # SentencePiece word-boundary marker

RELEASE_URL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models"
VAD_MODEL_FILE = "silero_vad.onnx"
STALE_TEMP_SEC = 15 * 60

ENGINES = {
    "nemotron-en-1120ms": {
        "kind": "online",
        "archive": "sherpa-onnx-nemotron-speech-streaming-en-0.6b-1120ms-int8-2026-04-25",
    },
    "nemotron-en-560ms": {
        "kind": "online",
        "archive": "sherpa-onnx-nemotron-speech-streaming-en-0.6b-560ms-int8-2026-04-25",
    },
    "parakeet-v3-vad": {
        "kind": "vad",
        "archive": "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8",
    },
}
DEFAULT_ENGINE = "nemotron-en-1120ms"

log = logging.getLogger("live_worker")


# ---------------------------------------------------------------------------
# Protocol output
# ---------------------------------------------------------------------------

def open_protocol_stream():
    """Return a UTF-8, line-buffered text stream on the real stdout.

    Native libraries (onnxruntime, sherpa-onnx) may print to fd 1. The real stdout is
    duplicated for the protocol and fd 1 is pointed at stderr, so stray output can
    never corrupt the JSON lines.
    """
    try:
        sys.stdout.flush()
        proto_fd = os.dup(sys.stdout.fileno())
        os.dup2(sys.stderr.fileno(), sys.stdout.fileno())
        stream = open(proto_fd, "w", encoding="utf-8", newline="\n", buffering=1)
        sys.stdout = sys.stderr
        return stream
    except (OSError, ValueError, AttributeError):
        sys.stdout.reconfigure(encoding="utf-8", newline="\n", line_buffering=True)
        return sys.stdout


class Emitter:
    """Writes protocol events as JSON lines."""

    def __init__(self, stream):
        self.stream = stream

    def emit(self, event_type, **fields):
        event = {"type": event_type}
        event.update(fields)
        self.stream.write(json.dumps(event, ensure_ascii=False, separators=(",", ":")) + "\n")
        self.stream.flush()


# ---------------------------------------------------------------------------
# Model download
# ---------------------------------------------------------------------------

def _mb(n):
    return n / (1024 * 1024)


def download(url, dest):
    """Download url to dest through a temporary name, then rename it into place."""
    name = os.path.basename(dest)
    tmp = f"{dest}.part-{os.getpid()}"
    log.info("downloading %s", url)
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "scriberr-live-worker"})
        with urllib.request.urlopen(req, timeout=60) as resp, open(tmp, "wb") as out:
            total = int(resp.headers.get("Content-Length") or 0)
            done = 0
            last_log = time.monotonic()
            while True:
                chunk = resp.read(1024 * 1024)
                if not chunk:
                    break
                out.write(chunk)
                done += len(chunk)
                now = time.monotonic()
                if now - last_log >= 2.0:
                    last_log = now
                    if total:
                        log.info("  %s: %.1f / %.1f MB (%.0f%%)", name, _mb(done), _mb(total), 100.0 * done / total)
                    else:
                        log.info("  %s: %.1f MB", name, _mb(done))
            out.flush()
            os.fsync(out.fileno())
        if total and done != total:
            raise IOError(f"incomplete download of {name}: {done} of {total} bytes")
        os.replace(tmp, dest)
        log.info("downloaded %s (%.1f MB)", name, _mb(done))
    finally:
        if os.path.exists(tmp):
            os.remove(tmp)


class _ProgressReader:
    """File wrapper that logs how much of a compressed archive has been read."""

    def __init__(self, f, total, label):
        self.f = f
        self.total = total
        self.label = label
        self.done = 0
        self.last_log = time.monotonic()

    def read(self, size=-1):
        data = self.f.read(size)
        self.done += len(data)
        now = time.monotonic()
        if now - self.last_log >= 2.0 and self.total:
            self.last_log = now
            log.info("  extracting %s: %.0f%%", self.label, 100.0 * self.done / self.total)
        return data


def extract_archive(archive_path, models_dir, name):
    """Extract a .tar.bz2 into a temporary directory, then rename the model dir into place."""
    tmp_dir = os.path.join(models_dir, f".{name}.extract-{os.getpid()}")
    shutil.rmtree(tmp_dir, ignore_errors=True)
    os.makedirs(tmp_dir)
    try:
        log.info("extracting %s", os.path.basename(archive_path))
        with open(archive_path, "rb") as f:
            reader = _ProgressReader(f, os.path.getsize(archive_path), name)
            with tarfile.open(fileobj=reader, mode="r|bz2") as tar:
                for member in tar:
                    if hasattr(tarfile, "data_filter"):
                        tar.extract(member, tmp_dir, filter="data")
                    else:
                        target = os.path.realpath(os.path.join(tmp_dir, member.name))
                        if not target.startswith(os.path.realpath(tmp_dir) + os.sep) or member.issym() or member.islnk():
                            raise IOError(f"unsafe path in archive: {member.name}")
                        tar.extract(member, tmp_dir)
        extracted = os.path.join(tmp_dir, name)
        if not os.path.isdir(extracted):
            raise IOError(f"archive did not contain {name}/")
        final = os.path.join(models_dir, name)
        if os.path.isdir(final):
            log.info("%s appeared while extracting; keeping the existing copy", name)
        else:
            os.replace(extracted, final)
        log.info("extracted %s", name)
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


def _pick(model_dir, prefix):
    hits = glob.glob(os.path.join(model_dir, f"{prefix}*.onnx"))
    # Prefer the int8 export when both precisions are present.
    hits.sort(key=lambda p: (".int8." not in os.path.basename(p), p))
    return hits[0] if hits else None


def transducer_files(model_dir):
    """Return the encoder/decoder/joiner/tokens paths, or None if any is missing."""
    files = {key: _pick(model_dir, key) for key in ("encoder", "decoder", "joiner")}
    files["tokens"] = os.path.join(model_dir, "tokens.txt")
    for path in files.values():
        if not path or not os.path.isfile(path) or os.path.getsize(path) == 0:
            return None
    return files


def remove_stale_temps(models_dir, name):
    """Remove temporary files and dirs that a killed worker left while fetching name."""
    now = time.time()
    for entry in os.listdir(models_dir):
        if not (entry.startswith(f".{name}.") or entry.startswith(f"{name}.part-")):
            continue
        path = os.path.join(models_dir, entry)
        try:
            # A live download or extraction keeps touching its files; leave those alone.
            if now - os.path.getmtime(path) < STALE_TEMP_SEC:
                continue
            if os.path.isdir(path):
                shutil.rmtree(path, ignore_errors=True)
            else:
                os.remove(path)
            log.info("removed stale %s", entry)
        except OSError:
            pass


def ensure_transducer(models_dir, archive):
    """Return the model files for archive, downloading and extracting it when missing."""
    model_dir = os.path.join(models_dir, archive)
    files = transducer_files(model_dir)
    if files:
        return files
    if os.path.isdir(model_dir):
        log.warning("%s is incomplete; downloading it again", model_dir)
        shutil.rmtree(model_dir, ignore_errors=True)
    os.makedirs(models_dir, exist_ok=True)
    remove_stale_temps(models_dir, archive)
    tmp_archive = os.path.join(models_dir, f".{archive}.tar.bz2.{os.getpid()}")
    try:
        download(f"{RELEASE_URL}/{archive}.tar.bz2", tmp_archive)
        extract_archive(tmp_archive, models_dir, archive)
    finally:
        if os.path.exists(tmp_archive):
            os.remove(tmp_archive)
    files = transducer_files(model_dir)
    if not files:
        raise FileNotFoundError(f"model files missing in {model_dir} after extraction")
    return files


def ensure_vad(models_dir):
    path = os.path.join(models_dir, VAD_MODEL_FILE)
    if not os.path.isfile(path) or os.path.getsize(path) == 0:
        os.makedirs(models_dir, exist_ok=True)
        remove_stale_temps(models_dir, VAD_MODEL_FILE)
        download(f"{RELEASE_URL}/{VAD_MODEL_FILE}", path)
    return path


# ---------------------------------------------------------------------------
# Results to words
# ---------------------------------------------------------------------------

def tokens_to_words(tokens, times, durations=None, end_limit=None):
    """Group SentencePiece tokens into words with start and end times.

    A token that starts with U+2581 starts a new word (sherpa-onnx results already
    turn it into a plain space, so both are accepted). times are absolute token
    start times. durations (TDT models) give each token's length; without them a
    word ends WORD_TAIL_SEC after its last token or at the next word's start.
    """
    words = []
    for i, (tok, t) in enumerate(zip(tokens, times)):
        piece = tok.replace(SP_SPACE, " ")
        tok_end = t + durations[i] if durations is not None and i < len(durations) else None
        if piece.startswith(" ") or not words:
            piece = piece.lstrip(" ")
            words.append({"word": piece, "start": t, "last": t, "end": tok_end})
        else:
            w = words[-1]
            w["word"] += piece
            w["last"] = t
            w["end"] = None if tok_end is None else max(tok_end, w["end"] if w["end"] is not None else t)
    words = [w for w in words if w["word"].strip()]

    # Streaming models often emit several words on the same encoder frame. Spread
    # such a run evenly up to the next word, so the words do not all share a start.
    i = 0
    while i < len(words):
        j = i
        while j + 1 < len(words) and words[j + 1]["start"] == words[i]["start"]:
            j += 1
        if j > i:
            t = words[i]["start"]
            nxt = words[j + 1]["start"] if j + 1 < len(words) else words[j]["last"] + WORD_TAIL_SEC
            step = max(nxt - t, 0.0) / (j - i + 1)
            for k in range(i, j + 1):
                words[k]["start"] = t + (k - i) * step
                words[k]["last"] = max(words[k]["last"], words[k]["start"])
        i = j + 1

    out = []
    for i, w in enumerate(words):
        start = max(0.0, w["start"])
        end = w["end"] if w["end"] is not None else w["last"] + WORD_TAIL_SEC
        if i + 1 < len(words):
            end = min(end, words[i + 1]["start"])
        if end_limit is not None:
            end = min(end, end_limit)
        end = max(end, start + 0.01)
        out.append({"word": w["word"].strip(), "start": round(start, 3), "end": round(end, 3)})
    return out


def _r(t):
    return round(max(0.0, t), 3)


# ---------------------------------------------------------------------------
# Engines
# ---------------------------------------------------------------------------

class NemotronEngine:
    """True streaming ASR: sherpa-onnx OnlineRecognizer with endpoint detection."""

    def __init__(self, files, threads, out, partials=True):
        import sherpa_onnx

        self.partials = partials
        self.rec = sherpa_onnx.OnlineRecognizer.from_transducer(
            tokens=files["tokens"],
            encoder=files["encoder"],
            decoder=files["decoder"],
            joiner=files["joiner"],
            num_threads=threads,
            sample_rate=SAMPLE_RATE,
            decoding_method="greedy_search",
            enable_endpoint_detection=True,
            rule1_min_trailing_silence=2.4,  # endpoint after 2.4 s of silence with no speech
            rule2_min_trailing_silence=0.8,  # endpoint after 0.8 s of silence following speech
            rule3_min_utterance_length=20.0,  # force an endpoint so segments stay bounded
        )
        self.out = out
        self.fed = 0  # real samples received, excluding preroll and padding
        self.finals = 0
        self.last_partial = ""
        self.last_partial_at = 0.0
        self.stream = self.rec.create_stream()
        self.stream.accept_waveform(SAMPLE_RATE, np.zeros(int(PREROLL_SEC * SAMPLE_RATE), dtype=np.float32))

    @property
    def processed_sec(self):
        return self.fed / SAMPLE_RATE

    def _session_time(self, t):
        """Stream time (which includes the preroll) to session time."""
        return t - PREROLL_SEC

    def _decode(self):
        while self.rec.is_ready(self.stream):
            self.rec.decode_stream(self.stream)

    def _token_times(self, result):
        # Token timestamps are relative to the current segment, which starts at
        # result.start_time on the stream clock.
        base = result.start_time
        return [self._session_time(base + t) for t in result.timestamps]

    def accept(self, samples):
        self.stream.accept_waveform(SAMPLE_RATE, samples)
        self.fed += len(samples)
        self._decode()
        if self.rec.is_endpoint(self.stream):
            self._emit_final()
            self.rec.reset(self.stream)
        elif self.partials:
            self._maybe_partial()

    def finish(self):
        self.stream.accept_waveform(SAMPLE_RATE, np.zeros(int(TAIL_PAD_SEC * SAMPLE_RATE), dtype=np.float32))
        self.stream.input_finished()
        self._decode()
        self._emit_final()

    def _maybe_partial(self):
        now = time.monotonic()
        if now - self.last_partial_at < PARTIAL_INTERVAL_SEC:
            return
        result = self.rec.get_result_all(self.stream)
        text = result.text.strip()
        if not text or text == self.last_partial:
            return
        times = self._token_times(result)
        start = times[0] if times else self._session_time(result.start_time)
        self.out.emit("partial", text=text, start=_r(start))
        self.last_partial = text
        self.last_partial_at = now

    def _emit_final(self):
        result = self.rec.get_result_all(self.stream)
        text = result.text.strip()
        self.last_partial = ""
        if not text:
            return
        now_sec = self.processed_sec
        times = self._token_times(result)
        words = tokens_to_words(list(result.tokens), times, end_limit=now_sec) if times else []
        if words:
            start, end = words[0]["start"], words[-1]["end"]
        else:
            start, end = self._session_time(result.start_time), now_sec
        self.out.emit("final", text=text, start=_r(start), end=_r(max(start, end)), words=words)
        self.finals += 1
        log.debug("final [%.2f-%.2f] %s", start, end, text)


class AudioHistory:
    """The most recent audio, addressed by absolute sample index."""

    def __init__(self, seconds):
        self.keep = int(seconds * SAMPLE_RATE)
        self.buf = np.zeros(2 * self.keep, dtype=np.float32)
        self.n = 0  # valid samples in buf
        self.start = 0  # absolute index of buf[0]

    @property
    def end(self):
        return self.start + self.n

    def append(self, samples):
        if self.n + len(samples) > len(self.buf):
            keep = min(self.n, self.keep)
            self.buf[:keep] = self.buf[self.n - keep:self.n]
            self.start += self.n - keep
            self.n = keep
        self.buf[self.n:self.n + len(samples)] = samples
        self.n += len(samples)

    def get(self, a, b):
        a = max(a, self.start)
        b = min(b, self.end)
        return self.buf[a - self.start:max(a, b) - self.start].copy()


class ParakeetVadEngine:
    """Pseudo-streaming ASR: Silero VAD cuts utterances, Parakeet TDT v3 decodes each one.

    Silero's max_speech_duration is soft (past it the VAD only needs a shorter, quieter
    pause), so continuous speech or crosstalk can run on for minutes. An utterance that
    reaches VAD_HARD_CUT_SEC is therefore cut at its quietest point near the end and
    finalized; the VAD segment that eventually closes only contributes the rest.
    """

    VAD_HARD_CUT_SEC = 15.0
    VAD_CUT_SEARCH_SEC = 5.0  # where to look for the quietest 30 ms before a hard cut
    HISTORY_SEC = 60.0
    PREPAD_SEC = 0.3  # silence before each utterance; Parakeet then gets the first word right more often

    def __init__(self, files, vad_path, threads, out, partials=True):
        import sherpa_onnx

        self.partials = partials
        self.rec = sherpa_onnx.OfflineRecognizer.from_transducer(
            encoder=files["encoder"],
            decoder=files["decoder"],
            joiner=files["joiner"],
            tokens=files["tokens"],
            num_threads=threads,
            sample_rate=SAMPLE_RATE,
            decoding_method="greedy_search",
            model_type="nemo_transducer",
        )
        cfg = sherpa_onnx.VadModelConfig()
        cfg.silero_vad.model = vad_path
        cfg.silero_vad.threshold = 0.5
        cfg.silero_vad.min_silence_duration = 0.5
        cfg.silero_vad.min_speech_duration = 0.25
        cfg.silero_vad.max_speech_duration = 15.0
        cfg.sample_rate = SAMPLE_RATE
        cfg.num_threads = 1
        self.vad = sherpa_onnx.VoiceActivityDetector(cfg, buffer_size_in_seconds=60)
        self.window = cfg.silero_vad.window_size
        self.pending = np.zeros(0, dtype=np.float32)  # samples short of a full VAD window
        self.history = AudioHistory(self.HISTORY_SEC)
        self.prepad = np.zeros(int(self.PREPAD_SEC * SAMPLE_RATE), dtype=np.float32)
        self.speech_start = None  # absolute sample where the utterance in progress began
        self.done_until = 0  # audio before this sample is already finalized
        self.out = out
        self.fed = 0
        self.finals = 0
        self.last_partial = ""
        self.last_partial_at = 0.0
        self.partial_interval = 1.0

    @property
    def processed_sec(self):
        return self.fed / SAMPLE_RATE

    def _decode(self, samples):
        """Decode one utterance. Token timestamps in the result include PREPAD_SEC."""
        st = self.rec.create_stream()
        st.accept_waveform(SAMPLE_RATE, np.concatenate([self.prepad, samples]))
        self.rec.decode_stream(st)
        return st.result

    def accept(self, samples):
        # The VAD sees every sample, so its sample indices are session sample indices.
        self.history.append(samples)
        self.fed += len(samples)
        buf = np.concatenate([self.pending, samples]) if len(self.pending) else samples
        n = len(buf) - len(buf) % self.window
        for i in range(0, n, self.window):
            self.vad.accept_waveform(buf[i:i + self.window])
        self.pending = buf[n:]
        self._drain()
        if self.vad.is_speech_detected():
            if self.speech_start is None:
                start = self.vad.current_segment.start
                self.speech_start = start if start >= 0 else self.fed
            self._in_progress()

    def finish(self):
        if len(self.pending):
            self.vad.accept_waveform(self.pending)
            self.pending = np.zeros(0, dtype=np.float32)
        self.vad.flush()
        self._drain()

    def _drain(self):
        while not self.vad.empty():
            seg = self.vad.front
            s0 = seg.start
            samples = np.asarray(seg.samples, dtype=np.float32)
            self.vad.pop()
            s1 = s0 + len(samples)
            a = max(s0, self.done_until)
            self.speech_start = None
            self.done_until = max(self.done_until, s1)
            if s1 - a >= SAMPLE_RATE // 10:
                self._emit_final(samples[a - s0:], a)

    def _in_progress(self):
        a = max(self.speech_start, self.done_until)
        if self.fed - a >= self.VAD_HARD_CUT_SEC * SAMPLE_RATE:
            lo = a + int((self.VAD_HARD_CUT_SEC - self.VAD_CUT_SEARCH_SEC) * SAMPLE_RATE)
            cut = self._quietest(lo, self.fed)
            log.debug("hard cut of a %.1fs utterance at %.2fs", (self.fed - a) / SAMPLE_RATE, cut / SAMPLE_RATE)
            self.done_until = cut
            self._emit_final(self.history.get(a, cut), a)
        elif self.partials:
            self._maybe_partial(a)

    def _quietest(self, lo, hi):
        """Absolute sample index in the middle of the quietest 30 ms frame of [lo, hi)."""
        audio = self.history.get(lo, hi)
        frame = SAMPLE_RATE * 3 // 100
        n = len(audio) // frame
        if n == 0:
            return hi
        energy = np.square(audio[:n * frame]).reshape(n, frame).mean(axis=1)
        return lo + int(np.argmin(energy)) * frame + frame // 2

    def _maybe_partial(self, a):
        """Decode the utterance in progress now and then, at no more than ~25% CPU."""
        now = time.monotonic()
        if now - self.last_partial_at < self.partial_interval:
            return
        if self.fed - a > (self.VAD_HARD_CUT_SEC - 3.0) * SAMPLE_RATE:
            return  # a hard cut is imminent; do not delay its final with a partial decode
        samples = self.history.get(a, self.fed)
        if len(samples) < SAMPLE_RATE // 2:
            return
        t0 = time.perf_counter()
        text = self._decode(samples).text.strip()
        cost = time.perf_counter() - t0
        self.partial_interval = max(1.0, 4.0 * cost)
        self.last_partial_at = time.monotonic()
        if text and text != self.last_partial:
            self.out.emit("partial", text=text, start=_r(a / SAMPLE_RATE))
            self.last_partial = text

    def _emit_final(self, samples, start_sample):
        self.last_partial = ""
        if len(samples) == 0:
            return
        seg_start = start_sample / SAMPLE_RATE
        result = self._decode(samples)
        text = result.text.strip()
        if not text:
            return
        seg_end = seg_start + len(samples) / SAMPLE_RATE
        times = [max(seg_start, seg_start + t - self.PREPAD_SEC) for t in result.timestamps]
        durations = list(result.durations) if len(result.durations) == len(times) else None
        words = tokens_to_words(list(result.tokens), times, durations, end_limit=seg_end) if times else []
        if words:
            start, end = words[0]["start"], words[-1]["end"]
        else:
            start, end = seg_start, seg_end
        self.out.emit("final", text=text, start=_r(start), end=_r(max(start, end)), words=words)
        self.finals += 1
        log.debug("final [%.2f-%.2f] %s", start, end, text)


def load_engine(args, out):
    spec = ENGINES[args.engine]
    models_dir = os.path.abspath(args.models_dir)
    files = ensure_transducer(models_dir, spec["archive"])
    log.info("loading %s from %s (threads=%d)", args.engine, os.path.dirname(files["encoder"]), args.threads)
    t0 = time.perf_counter()
    if spec["kind"] == "online":
        engine = NemotronEngine(files, args.threads, out, partials=args.partials)
    else:
        engine = ParakeetVadEngine(files, ensure_vad(models_dir), args.threads, out, partials=args.partials)
    log.info("model loaded in %.1fs", time.perf_counter() - t0)
    return engine


# ---------------------------------------------------------------------------
# Main loop
# ---------------------------------------------------------------------------

def pcm_to_float(data):
    return np.frombuffer(data, dtype="<i2").astype(np.float32) / 32768.0


def run(engine, out, stdin):
    """Feed stdin to the engine until EOF, then flush."""
    read = getattr(stdin, "read1", stdin.read)
    pending = bytearray()
    compute = 0.0
    started = time.monotonic()
    last_progress = started
    last_stats = started

    while True:
        chunk = read(READ_SIZE)
        if not chunk:
            break
        pending += chunk
        n = len(pending) // BLOCK_BYTES
        if not n:
            continue
        samples = pcm_to_float(bytes(pending[:n * BLOCK_BYTES]))
        del pending[:n * BLOCK_BYTES]
        for i in range(n):
            t0 = time.perf_counter()
            engine.accept(samples[i * BLOCK_SAMPLES:(i + 1) * BLOCK_SAMPLES])
            compute += time.perf_counter() - t0
            now = time.monotonic()
            if now - last_progress >= PROGRESS_INTERVAL_SEC:
                out.emit("progress", processed_sec=round(engine.processed_sec, 2))
                last_progress = now
            if now - last_stats >= STATS_INTERVAL_SEC:
                audio = engine.processed_sec
                log.info("audio %.0fs, compute RTF %.3f, %d finals", audio, compute / audio, engine.finals)
                last_stats = now

    # EOF: decode what is left (an odd trailing byte cannot be a sample).
    usable = len(pending) - len(pending) % 2
    t0 = time.perf_counter()
    if usable:
        engine.accept(pcm_to_float(bytes(pending[:usable])))
    engine.finish()
    compute += time.perf_counter() - t0
    audio = engine.processed_sec
    out.emit("progress", processed_sec=round(audio, 2))
    log.info("stdin closed: audio %.1fs in %.1fs wall, compute RTF %.3f, %d finals",
             audio, time.monotonic() - started, compute / audio if audio else 0.0, engine.finals)
    out.emit("eof")


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description="Live ASR worker: s16le 16 kHz mono PCM on stdin, JSON lines on stdout"
    )
    parser.add_argument("--engine", choices=sorted(ENGINES), default=DEFAULT_ENGINE,
                        help=f"ASR engine (default: {DEFAULT_ENGINE})")
    parser.add_argument("--models-dir", default=os.path.join("data", "live-models"),
                        help="Directory holding the k2-fsa model archives (default: data/live-models)")
    parser.add_argument("--threads", type=int, default=4,
                        help="ONNX threads for the recognizer (default: 4)")
    parser.add_argument("--partials", action=argparse.BooleanOptionalAction, default=True,
                        help="emit partial text (default: on). For parakeet-v3-vad each partial "
                             "re-decodes the utterance so far, which costs CPU")
    parser.add_argument("--log-level", default="info", type=str.lower,
                        choices=["debug", "info", "warning", "error"],
                        help="stderr log level (default: info)")
    args = parser.parse_args(argv)
    args.threads = max(1, args.threads)
    return args


def main(argv=None):
    args = parse_args(argv)
    try:
        sys.stderr.reconfigure(encoding="utf-8", errors="backslashreplace")
    except (AttributeError, ValueError):
        pass
    logging.basicConfig(
        level=getattr(logging, args.log_level.upper()),
        format="%(asctime)s live_worker %(levelname)s %(message)s",
        stream=sys.stderr,
    )
    out = Emitter(open_protocol_stream())

    try:
        import sherpa_onnx

        log.info("sherpa-onnx %s, engine %s", sherpa_onnx.__version__, args.engine)
        engine = load_engine(args, out)
        out.emit("ready", engine=args.engine, sample_rate=SAMPLE_RATE)
        run(engine, out, sys.stdin.buffer)
    except KeyboardInterrupt:
        return 130
    except BrokenPipeError:
        log.error("stdout closed by the reader; exiting")
        return 1
    except Exception as e:
        log.exception("fatal error")
        try:
            out.emit("error", message=f"{type(e).__name__}: {e}")
        except Exception:
            pass
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
