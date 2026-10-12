#!/usr/bin/env python3
"""Measure local context construction. Output contains metrics and hashes, never transcript text."""
from __future__ import annotations

import argparse
import hashlib
import json
import statistics
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from agent_decisions import evidence, transcript


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--transcript", type=Path, required=True)
    parser.add_argument("--host", choices=("codex", "claude"), required=True)
    parser.add_argument("--cwd", type=Path, required=True)
    parser.add_argument("--query", default="SDK request ownership lifecycle helper")
    parser.add_argument("--samples", type=int, choices=range(1, 11), default=3)
    parser.add_argument("--through-bytes", type=int)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    size = args.through_bytes if args.through_bytes is not None else args.transcript.stat().st_size
    if size <= 0:
        parser.error("--through-bytes must be positive")
    started = time.perf_counter()
    with args.transcript.open("rb") as handle:
        lines = handle.read(size).decode("utf-8", errors="replace").splitlines()
    records = []
    for line in lines:
        try:
            row = json.loads(line)
        except ValueError:
            continue
        if isinstance(row, dict):
            records.append(row)
    parsed = {"codex": transcript._codex, "claude": transcript._claude}[args.host](records)
    parse_ms = (time.perf_counter() - started) * 1_000
    samples = []
    for _ in range(args.samples):
        started = time.perf_counter()
        bundle = evidence.build(parsed, query=args.query, cwd=args.cwd)
        samples.append((time.perf_counter() - started) * 1_000)
    encoded = json.dumps(bundle, sort_keys=True).encode()
    result = {"transcript_bytes": size, "tool_calls": len(parsed.tool_calls), "parse_ms": round(parse_ms, 3),
              "evidence_ms": [round(value, 3) for value in samples],
              "evidence_median_ms": round(statistics.median(samples), 3), "api_calls": 0,
              "evidence_bytes": len(encoded), "evidence_sha256": hashlib.sha256(encoded).hexdigest()}
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
