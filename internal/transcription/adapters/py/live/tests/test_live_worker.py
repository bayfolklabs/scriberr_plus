#!/usr/bin/env python3
"""
End-to-end protocol test for live_worker.py. Standard library only, no pytest.

The worker runs through `uv run --project <env>` the way the Go server starts it, on
the AMI test clip:

  ffmpeg      ffmpeg -> worker pipe (the real pipeline): ready, finals, progress, eof
  odd-writes  the same PCM written in odd-sized pieces: identical finals
  empty       stdin closed at once: ready, progress, eof and no finals
  silence     4 s of digital silence: no finals, clean eof
  realtime    (--realtime) 100 ms writes every 100 ms; reports final latency

Usage (from the repo root):
  python internal/transcription/adapters/py/live/tests/test_live_worker.py
      [--engine nemotron-en-1120ms | --engine all] [--models-dir data/live-models]
      [--env data/whisperx-env/live] [--realtime] [--dump DIR]

Exit code 0 when every check passes.
"""

import argparse
import json
import os
import random
import subprocess
import sys
import threading
import time
import wave
from pathlib import Path

LIVE_DIR = Path(__file__).resolve().parent.parent
PROJECT_ROOT = Path(__file__).resolve().parents[6]
WORKER = LIVE_DIR / "live_worker.py"
AUDIO_FILE = PROJECT_ROOT / "tests" / "data" / "AMI-Corpus-IB4002.Mix-Headset-clip.wav"
EXPECTED_PHRASES = ["desktop computer", "reading room"]

ENGINE_DIRS = {
    "nemotron-en-1120ms": "sherpa-onnx-nemotron-speech-streaming-en-0.6b-1120ms-int8-2026-04-25",
    "nemotron-en-560ms": "sherpa-onnx-nemotron-speech-streaming-en-0.6b-560ms-int8-2026-04-25",
    "parakeet-v3-vad": "sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8",
}
EVENT_TYPES = {"ready", "partial", "final", "progress", "eof", "error"}

SAMPLE_RATE = 16000
BLOCK_BYTES = SAMPLE_RATE // 10 * 2


class CheckFailed(Exception):
    pass


def check(cond, msg):
    if not cond:
        raise CheckFailed(msg)


class Worker:
    """A running worker with its stdout events (and arrival times) collected in threads."""

    def __init__(self, args, engine, stdin=subprocess.PIPE):
        cmd = [
            "uv", "run", "--project", str(args.env),
            "python", str(WORKER),
            "--engine", engine,
            "--models-dir", str(args.models_dir),
            "--threads", str(args.threads),
            *args.worker_arg,
        ]
        self.proc = subprocess.Popen(cmd, stdin=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.events = []  # (monotonic arrival, event dict)
        self.bad_lines = []
        self.stderr = []
        self.ready = threading.Event()
        self._threads = [
            threading.Thread(target=self._read_stdout, daemon=True),
            threading.Thread(target=self._read_stderr, daemon=True),
        ]
        for t in self._threads:
            t.start()

    def _read_stdout(self):
        for raw in self.proc.stdout:
            now = time.monotonic()
            try:
                line = raw.decode("utf-8")
                event = json.loads(line)
                check(isinstance(event, dict), "not an object")
            except Exception:
                self.bad_lines.append(raw)
                continue
            self.events.append((now, event))
            if event.get("type") in ("ready", "error"):
                self.ready.set()
        self.ready.set()

    def _read_stderr(self):
        for raw in self.proc.stderr:
            self.stderr.append(raw.decode("utf-8", "replace").rstrip())

    def write(self, data):
        self.proc.stdin.write(data)
        self.proc.stdin.flush()

    def close_stdin(self):
        if self.proc.stdin:
            self.proc.stdin.close()

    def wait(self, timeout=600):
        code = self.proc.wait(timeout=timeout)
        for t in self._threads:
            t.join(timeout=10)
        return code

    def of_type(self, kind):
        return [e for _, e in self.events if e.get("type") == kind]


def read_pcm(path):
    with wave.open(str(path), "rb") as w:
        check(w.getframerate() == SAMPLE_RATE and w.getnchannels() == 1 and w.getsampwidth() == 2,
              f"{path} must be 16 kHz mono s16")
        return w.readframes(w.getnframes())


def check_protocol(worker, code, engine, audio_sec, expect_speech):
    """Assert the protocol rules on a finished worker run."""
    tail = "\n".join(worker.stderr[-15:])
    check(code == 0, f"exit code {code}; stderr tail:\n{tail}")
    check(not worker.bad_lines, f"non-JSON stdout lines: {worker.bad_lines[:3]}")
    events = [e for _, e in worker.events]
    check(events, "no events")
    for e in events:
        check(e.get("type") in EVENT_TYPES, f"unknown event type: {e}")
    check(not worker.of_type("error"), f"error event: {worker.of_type('error')}")

    check(events[0]["type"] == "ready", f"first event is not ready: {events[0]}")
    check(events[0].get("engine") == engine, f"ready.engine is {events[0].get('engine')}")
    check(events[0].get("sample_rate") == SAMPLE_RATE, "ready.sample_rate is not 16000")
    check(sum(1 for e in events if e["type"] == "ready") == 1, "more than one ready")
    check(events[-1]["type"] == "eof", f"last event is not eof: {events[-1]}")
    check(sum(1 for e in events if e["type"] == "eof") == 1, "more than one eof")

    for p in worker.of_type("partial"):
        check(isinstance(p.get("text"), str) and p["text"].strip(), f"empty partial: {p}")
        check(isinstance(p.get("start"), (int, float)) and p["start"] >= 0, f"bad partial.start: {p}")

    finals = worker.of_type("final")
    prev_start = -1.0
    for f in finals:
        check(isinstance(f.get("text"), str) and f["text"].strip(), f"empty final: {f}")
        s, e = f.get("start"), f.get("end")
        check(isinstance(s, (int, float)) and isinstance(e, (int, float)), f"final without times: {f}")
        check(0 <= s <= e <= audio_sec + 0.05, f"final times out of range (audio {audio_sec:.2f}s): {f}")
        check(s >= prev_start, f"finals out of order: {f}")
        prev_start = s
        words = f.get("words")
        check(isinstance(words, list), f"final.words is not a list: {f}")
        prev_w = -1.0
        for w in words:
            check(isinstance(w.get("word"), str) and w["word"], f"bad word: {w}")
            check(s - 0.01 <= w["start"] <= w["end"] <= e + 0.01, f"word outside its segment: {w} in [{s}, {e}]")
            check(w["start"] >= prev_w, f"words out of order: {w}")
            prev_w = w["start"]
        if words:
            joined = "".join(w["word"] for w in words)
            check(joined == f["text"].replace(" ", ""), f"words do not spell the text: {joined!r} vs {f['text']!r}")

    progress = [p["processed_sec"] for p in worker.of_type("progress")]
    check(progress, "no progress events")
    check(all(b >= a for a, b in zip(progress, progress[1:])), "progress went backwards")
    check(abs(progress[-1] - audio_sec) < 0.01, f"last progress {progress[-1]} != audio {audio_sec:.2f}")

    text = " ".join(f["text"] for f in finals)
    if expect_speech:
        check(finals, "no final segments")
        low = text.lower()
        for phrase in EXPECTED_PHRASES:
            check(phrase in low, f"expected {phrase!r} in transcript: {text!r}")
    return text


def dump(args, name, worker):
    if not args.dump:
        return
    os.makedirs(args.dump, exist_ok=True)
    path = os.path.join(args.dump, f"{name}.jsonl")
    with open(path, "w", encoding="utf-8") as f:
        for _, e in worker.events:
            f.write(json.dumps(e, ensure_ascii=False) + "\n")
    with open(os.path.join(args.dump, f"{name}.stderr.log"), "w", encoding="utf-8") as f:
        f.write("\n".join(worker.stderr) + "\n")


def test_ffmpeg(args, engine, pcm):
    ff = subprocess.Popen(
        ["ffmpeg", "-hide_banner", "-loglevel", "error", "-i", str(AUDIO_FILE),
         "-f", "s16le", "-ac", "1", "-ar", str(SAMPLE_RATE), "pipe:1"],
        stdout=subprocess.PIPE,
    )
    worker = Worker(args, engine, stdin=ff.stdout)
    ff.stdout.close()  # the worker holds the only read end, so it sees EOF when ffmpeg exits
    code = worker.wait()
    check(ff.wait() == 0, "ffmpeg failed")
    dump(args, f"{engine}-ffmpeg", worker)
    text = check_protocol(worker, code, engine, len(pcm) / 2 / SAMPLE_RATE, expect_speech=True)
    finals = worker.of_type("final")
    return f"{len(finals)} finals, {len(worker.of_type('partial'))} partials: {text}", finals


def test_odd_writes(args, engine, pcm, reference):
    worker = Worker(args, engine)
    rng = random.Random(7)
    sizes = [1, 3, 5, 7, 333, 1601, 3199, 3201, 4097, 9999]
    pos = 0
    while pos < len(pcm):
        n = rng.choice(sizes)
        worker.write(pcm[pos:pos + n])
        pos += n
    worker.close_stdin()
    code = worker.wait()
    dump(args, f"{engine}-odd", worker)
    check_protocol(worker, code, engine, len(pcm) / 2 / SAMPLE_RATE, expect_speech=True)
    got = [(f["text"], f["start"], f["end"]) for f in worker.of_type("final")]
    want = [(f["text"], f["start"], f["end"]) for f in reference]
    check(got == want, f"finals differ from the ffmpeg run:\n{got}\nvs\n{want}")
    return f"{len(got)} finals identical to the ffmpeg run"


def test_empty(args, engine):
    worker = Worker(args, engine)
    worker.close_stdin()
    code = worker.wait()
    dump(args, f"{engine}-empty", worker)
    check_protocol(worker, code, engine, 0.0, expect_speech=False)
    check(not worker.of_type("final"), "final on empty input")
    return "events: " + " ".join(e["type"] for _, e in worker.events)


def test_silence(args, engine):
    worker = Worker(args, engine)
    worker.write(b"\x00" * (4 * SAMPLE_RATE * 2))
    worker.close_stdin()
    code = worker.wait()
    dump(args, f"{engine}-silence", worker)
    check_protocol(worker, code, engine, 4.0, expect_speech=False)
    check(not worker.of_type("final"), f"final on silence: {worker.of_type('final')}")
    return f"{len(worker.of_type('progress'))} progress events, eof"


def test_realtime(args, engine, pcm):
    """Write 100 ms of audio every 100 ms and measure when finals arrive."""
    pcm = pcm + b"\x00" * int(args.tail_silence * SAMPLE_RATE) * 2
    worker = Worker(args, engine)
    check(worker.ready.wait(600), "worker never became ready")
    t0 = time.monotonic()
    blocks = range(0, len(pcm), BLOCK_BYTES)
    for i, pos in enumerate(blocks):
        delay = t0 + (i + 1) * 0.1 - time.monotonic()  # block i is complete at (i+1)*100 ms
        if delay > 0:
            time.sleep(delay)
        worker.write(pcm[pos:pos + BLOCK_BYTES])
    closed_at = time.monotonic()
    worker.close_stdin()
    code = worker.wait()
    dump(args, f"{engine}-realtime", worker)
    check_protocol(worker, code, engine, len(pcm) / 2 / SAMPLE_RATE, expect_speech=True)

    lines = []
    delays = []
    first_partial = None
    for arrived, e in worker.events:
        if e["type"] == "partial" and first_partial is None:
            first_partial = (arrived - t0, e)
        if e["type"] != "final":
            continue
        delay = arrived - (t0 + e["end"])
        after_eof = arrived >= closed_at
        if not after_eof:
            delays.append(delay)
        lines.append(f"    end={e['end']:6.2f}s arrived={arrived - t0:6.2f}s delay={delay:5.2f}s"
                     f"{' (after EOF flush)' if after_eof else ''}  {e['text'][:60]}")
    summary = [f"stdin closed at {closed_at - t0:.2f}s, worker exited at {time.monotonic() - t0:.2f}s"]
    if first_partial:
        summary.append(f"first partial at {first_partial[0]:.2f}s: {first_partial[1]['text'][:60]!r}")
    if delays:
        summary.append(f"endpoint finals: mean delay {sum(delays) / len(delays):.2f}s, max {max(delays):.2f}s")
    return "\n".join(["", *["    " + s for s in summary], *lines])


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--engine", action="append",
                        help="engine to test (repeatable), or 'all' for every engine whose model is present")
    parser.add_argument("--models-dir", type=Path, default=PROJECT_ROOT / "data" / "live-models")
    parser.add_argument("--env", type=Path, default=PROJECT_ROOT / "data" / "whisperx-env" / "live",
                        help="uv project of the live env")
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--realtime", action="store_true", help="also run the real-time pacing test")
    parser.add_argument("--tail-silence", type=float, default=2.0,
                        help="seconds of silence after the clip in the real-time test (default 2)")
    parser.add_argument("--skip", default="", help="comma-separated tests to skip: ffmpeg,odd-writes,empty,silence")
    parser.add_argument("--dump", help="write each run's events (.jsonl) and stderr here")
    parser.add_argument("--worker-arg", action="append", default=[],
                        help="extra worker argument (repeatable), e.g. --worker-arg=--no-partials")
    args = parser.parse_args()

    engines = args.engine or ["nemotron-en-1120ms"]
    if "all" in engines:
        engines = [e for e, d in ENGINE_DIRS.items() if (args.models_dir / d).is_dir()]
    skip = set(filter(None, args.skip.split(",")))
    check(AUDIO_FILE.exists(), f"audio file not found: {AUDIO_FILE}")
    pcm = read_pcm(AUDIO_FILE)

    failures = 0
    for engine in engines:
        print(f"== {engine}", flush=True)
        reference = None
        tests = [
            ("ffmpeg", lambda: test_ffmpeg(args, engine, pcm)),
            ("odd-writes", lambda: test_odd_writes(args, engine, pcm, reference)),
            ("empty", lambda: test_empty(args, engine)),
            ("silence", lambda: test_silence(args, engine)),
        ]
        if args.realtime:
            tests.append(("realtime", lambda: test_realtime(args, engine, pcm)))
        for name, fn in tests:
            if name in skip or (name == "odd-writes" and reference is None):
                print(f"  SKIP {name}", flush=True)
                continue
            t0 = time.monotonic()
            try:
                result = fn()
                if name == "ffmpeg":
                    result, reference = result
                print(f"  PASS {name} ({time.monotonic() - t0:.1f}s): {result}", flush=True)
            except Exception as e:
                failures += 1
                print(f"  FAIL {name} ({time.monotonic() - t0:.1f}s): {type(e).__name__}: {e}", flush=True)
    print("OK" if not failures else f"{failures} FAILED", flush=True)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
