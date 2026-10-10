#!/usr/bin/env python3
"""Entry point named in hooks.json: hook.py <event> --host claude|codex, payload on stdin."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from agent_decisions.hook import main  # noqa: E402

sys.exit(main())
