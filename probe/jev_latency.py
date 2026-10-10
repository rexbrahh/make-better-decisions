#!/usr/bin/env python3
"""Throwaway probe: does the API accept the pinned model, and how fast is a full-size state?"""
import json
import os
import time
import urllib.error
import urllib.request
from pathlib import Path

key = os.environ.get("TYPESAFE_API_KEY") or (
    Path.home() / ".config/sops-nix/secrets/typesafe_api_key").read_text().strip()
evidence = "\n".join(f"line {i}: the tile cache re-reads tiles in bursts during panning" for i in range(1300))
body = json.dumps({
    "model": "jev-1.13.0",
    "state": {"evidence": evidence[:80_000]},
    "questions": {"bursty": {"type": "noul", "instructions": "Does `evidence` say tiles are re-read in bursts?"}},
}).encode()
for attempt in range(3):
    request = urllib.request.Request(
        "https://api.typesafe.ai/v1/systemone", data=body,
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    start = time.monotonic()
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            reply = json.loads(response.read())
        print(f"attempt {attempt}: {time.monotonic() - start:.2f}s model={reply['model']} usage={reply['usage']}")
    except urllib.error.HTTPError as error:
        print(f"attempt {attempt}: HTTP {error.code} {error.read()[:300]!r}")
