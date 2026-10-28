package adapters

import _ "embed"

// Live ASR worker assets. The live session pipeline (internal/live) writes these
// into <WHISPERX_ENV>/live/ and runs the worker there with `uv run`.

//go:embed py/live/live_worker.py
var LiveWorkerScript []byte

//go:embed py/live/pyproject.toml
var LivePyproject []byte
