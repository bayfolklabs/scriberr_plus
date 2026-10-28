#!/usr/bin/env python3
"""
Model download and extraction test for live_worker.py. No pytest needed.

Serves a small fake model archive from a local HTTP server and checks that the worker
downloads, extracts and renames it into place, leaves no temporary files behind, and
fails cleanly on a missing archive. --online also checks that every real archive URL
resolves (HEAD only) and downloads the real silero_vad.onnx into a temp dir.

Usage (numpy must be importable, e.g. inside the live env):
  uv run --project data/whisperx-env/live python internal/transcription/adapters/py/live/tests/test_model_download.py [--online]
"""

import argparse
import functools
import importlib.util
import io
import logging
import os
import sys
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

WORKER = Path(__file__).resolve().parent.parent / "live_worker.py"


def load_worker():
    spec = importlib.util.spec_from_file_location("live_worker", WORKER)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def make_archive(path, name):
    files = {
        "tokens.txt": b"<blk> 0\n",
        "encoder.int8.onnx": b"encoder",
        "decoder.int8.onnx": b"decoder",
        "joiner.int8.onnx": b"joiner",
        "README.md": b"fake model",
    }
    with tarfile.open(path, "w:bz2") as tar:
        info = tarfile.TarInfo(name)
        info.type = tarfile.DIRTYPE
        info.mode = 0o755
        tar.addfile(info)
        for fname, data in files.items():
            info = tarfile.TarInfo(f"{name}/{fname}")
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))


class QuietHandler(SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


def serve(directory):
    handler = functools.partial(QuietHandler, directory=directory)
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def leftovers(models_dir):
    return [n for n in os.listdir(models_dir) if n.startswith(".") or ".part-" in n]


def test_local(lw):
    with tempfile.TemporaryDirectory() as tmp:
        served = os.path.join(tmp, "served")
        models = os.path.join(tmp, "models")
        os.makedirs(served)
        make_archive(os.path.join(served, "fake-model.tar.bz2"), "fake-model")
        with open(os.path.join(served, lw.VAD_MODEL_FILE), "wb") as f:
            f.write(b"vad")
        server = serve(served)
        try:
            lw.RELEASE_URL = f"http://127.0.0.1:{server.server_address[1]}"

            # Temp files of a killed worker are removed once stale; fresh ones (a live
            # download in another worker) are kept.
            os.makedirs(os.path.join(models, ".fake-model.extract-99999"))
            stale = os.path.join(models, ".fake-model.tar.bz2.99999.part-99999")
            fresh = os.path.join(models, ".fake-model.tar.bz2.88888.part-88888")
            for path in (stale, fresh):
                with open(path, "wb") as f:
                    f.write(b"partial")
            old = time.time() - 2 * lw.STALE_TEMP_SEC
            os.utime(stale, (old, old))
            os.utime(os.path.join(models, ".fake-model.extract-99999"), (old, old))

            files = lw.ensure_transducer(models, "fake-model")
            assert files and os.path.basename(files["encoder"]) == "encoder.int8.onnx", files
            assert leftovers(models) == [os.path.basename(fresh)], leftovers(models)
            os.remove(fresh)
            assert sorted(os.listdir(models)) == ["fake-model"], os.listdir(models)

            # A second call finds the model without downloading.
            os.remove(os.path.join(served, "fake-model.tar.bz2"))
            assert lw.ensure_transducer(models, "fake-model") == files

            # An incomplete model dir is replaced.
            os.remove(files["joiner"])
            make_archive(os.path.join(served, "fake-model.tar.bz2"), "fake-model")
            assert lw.ensure_transducer(models, "fake-model")["joiner"] == files["joiner"]

            vad = lw.ensure_vad(models)
            assert open(vad, "rb").read() == b"vad"

            try:
                lw.ensure_transducer(models, "missing-model")
                raise AssertionError("missing archive did not raise")
            except urllib.error.HTTPError as e:
                assert e.code == 404, e
            assert not leftovers(models), leftovers(models)
            assert not os.path.exists(os.path.join(models, "missing-model"))
        finally:
            server.shutdown()
    return "download, extract, stale-temp cleanup, reuse, repair and 404 handling OK"


def test_online(lw):
    lines = []
    for engine, spec in lw.ENGINES.items():
        url = f"{lw.RELEASE_URL}/{spec['archive']}.tar.bz2"
        req = urllib.request.Request(url, method="HEAD", headers={"User-Agent": "scriberr-live-worker"})
        with urllib.request.urlopen(req, timeout=60) as resp:
            size = int(resp.headers.get("Content-Length") or 0)
            assert resp.status == 200 and size > 100 * 1024 * 1024, (engine, resp.status, size)
        lines.append(f"{engine}: {size / 1024 / 1024:.0f} MB")
    with tempfile.TemporaryDirectory() as tmp:
        vad = lw.ensure_vad(tmp)
        size = os.path.getsize(vad)
        assert size > 100 * 1024, size
        assert not leftovers(tmp), leftovers(tmp)
    lines.append(f"silero_vad.onnx downloaded: {size} bytes")
    return "; ".join(lines)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--online", action="store_true", help="also check the real k2-fsa URLs")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="  log: %(message)s", stream=sys.stderr)

    lw = load_worker()
    real_url = lw.RELEASE_URL
    tests = [("local", test_local)]
    if args.online:
        tests.append(("online", test_online))
    failures = 0
    for name, fn in tests:
        lw.RELEASE_URL = real_url
        try:
            print(f"PASS {name}: {fn(lw)}", flush=True)
        except Exception as e:
            failures += 1
            print(f"FAIL {name}: {type(e).__name__}: {e}", flush=True)
    print("OK" if not failures else f"{failures} FAILED", flush=True)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
