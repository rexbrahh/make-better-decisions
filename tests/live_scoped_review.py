#!/usr/bin/env python3
"""Real-Jev scoped review with controlled native-shaped fixtures, not live host activation."""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from agent_decisions import hook, ledger
from tests.helpers import codex_call, codex_output, codex_user, write_jsonl
from tests.test_receipts import native_metadata

LEASE_SOURCE = '''class Lease:
    def __init__(self): self.active = True
    @property
    def data(self):
        if not self.active: raise RuntimeError("released input")
        return "input"
    def release(self): self.active = False
    def __enter__(self): return self
    def __exit__(self, *args): self.release()
class Owner:
    def acquire(self): return Lease()
def decode(data): return data
'''


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    results = []
    calls = []
    saved_state = os.environ.get("AGENT_DECISIONS_STATE")
    try:
        with tempfile.TemporaryDirectory(prefix="agent-decisions-scoped-live-") as folder:
            root = Path(folder).resolve()
            os.environ["AGENT_DECISIONS_STATE"] = str(root / "state")
            run_fixture(root, results)
            calls = [call for row in ledger.entries("live-parent")
                     for call in row.get("metrics", {}).get("api_calls", ())]
    finally:
        if saved_state is None:
            os.environ.pop("AGENT_DECISIONS_STATE", None)
        else:
            os.environ["AGENT_DECISIONS_STATE"] = saved_state
    report = {"passed": sum(row["passed"] for row in results), "total": len(results),
              "api_calls": len(calls), "api_errors": [call["error"] for call in calls if "error" in call],
              "results": results, "metrics": calls}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({key: value for key, value in report.items() if key != "metrics"}))
    return int(bool(report["api_errors"]) or any(not row["passed"] for row in results))


def run_fixture(root: Path, results: list[dict]) -> None:
    (root / "lease.py").write_text(LEASE_SOURCE)
    source = root / "parser.py"
    source.write_text("from lease import decode\ndef parse(owner):\n    lease = owner.acquire()\n"
                      "    lease.release()\n    return decode(lease.data)\n")
    user = "Choose a lease handling control structure for the parser. Keep the input lease active while decode reads its data."
    rows = [native_metadata("live-parent", "live-parent", root), codex_user(user)]
    parent_path = write_jsonl(root, "parent.jsonl", rows)
    request = {"decision": "Which control structure should parser.py use with the Lease API in lease.py?",
               "options": [{"id": "finally", "description": "Use parse with try/finally to release the acquired lease after decode."},
                           {"id": "with", "description": "Use parse with the Lease context manager to release the acquired lease after decode."}],
               "scope": {"paths": ["parser.py", "tests/test_parser.py"],
                         "invariants": ["The input lease stays active while decode reads its data."]}}
    command = "ts-decide <<'EOF'\n" + json.dumps(request) + "\nEOF"
    payload = {"session_id": "live-parent", "tool_use_id": "review", "tool_name": "Bash",
               "tool_input": {"command": command}, "transcript_path": str(parent_path), "cwd": str(root)}
    response = hook.handle("pre_tool_use", payload, "codex")
    transport = response["hookSpecificOutput"]["updatedInput"]["command"]
    rendered = subprocess.run(["/bin/sh", "-c", transport], capture_output=True, text=True, check=True).stdout
    rows.extend([codex_call("review", "Bash", {"command": command}), codex_output("review", rendered)])
    write_jsonl(root, "parent.jsonl", rows)
    parent = hook._context({**payload, "tool_use_id": "context"}, "codex")
    receipt = next(row for row in parent.entries if row.get("gate") == "decide")
    ready = receipt["action"] == "advisory" and bool(receipt.get("receipt_id"))
    results.append({"case": "real scoped invariant review", "action": receipt["action"],
                    "pick": receipt.get("pick"), "eligible_options": receipt.get("eligible_options"), "passed": ready,
                    **({"verdict": rendered} if not ready else {})})
    if not ready:
        return
    marker = ("Implement the accepted parse control structure, then correct its test assertions.\nDecision receipt: "
              + receipt["receipt_id"] + "\nAccepted option: " + receipt["pick"])
    rows.extend([codex_call("spawn", "spawn_agent", {"message": marker, "task_name": "worker"}),
                 codex_output("spawn", json.dumps({"task_name": "/root/worker"}))])
    write_jsonl(root, "parent.jsonl", rows)
    child_path = write_jsonl(root, "child.jsonl", [
        native_metadata("live-child", "live-parent", root, parent="live-parent", agent="/root/worker"),
        codex_user("<codex_delegation>" + marker + "</codex_delegation>")])
    value = {"file_path": str(source), "old_string": source.read_text()}
    payload = {"session_id": "live-child", "tool_use_id": "edit", "tool_name": "Edit", "tool_input": value,
               "transcript_path": str(child_path), "cwd": str(root)}
    if receipt["pick"] == "finally":
        changed = ("from lease import decode\ndef parse(owner):\n    lease = owner.acquire()\n    try:\n"
                   "        return decode(lease.data)\n    finally:\n        lease.release()\n")
    else:
        changed = "from lease import decode\ndef parse(owner):\n    with owner.acquire() as lease:\n        return decode(lease.data)\n"
    value["new_string"] = changed
    inherited = hook._context(payload, "codex")
    results.append({"case": "native-shaped parent receipt inheritance", "passed": len(inherited.inherited_entries) == 1})
    response = hook.handle("pre_tool_use", payload, "codex")
    allowed = (response or {}).get("hookSpecificOutput", {}).get("permissionDecision") != "deny"
    results.append({"case": "real inherited scoped implementation", "passed": allowed, "response": response})
    if not allowed:
        return
    source.write_text(changed)
    check = subprocess.run([sys.executable, "-B", "-c", "from parser import parse; from lease import Owner; assert parse(Owner()) == 'input'"],
                           cwd=root, capture_output=True, text=True)
    results.append({"case": "fixture implementation executes", "passed": check.returncode == 0, "exit_code": check.returncode})
    payload = {**payload, "tool_use_id": "test-edit", "tool_input": {"file_path": str(root / "tests/test_parser.py"),
               "old_string": "assert result == None", "new_string": "assert result == \"input\""}}
    response = hook.handle("pre_tool_use", payload, "codex")
    allowed = (response or {}).get("hookSpecificOutput", {}).get("permissionDecision") != "deny"
    results.append({"case": "routine assertion correction after source changed", "passed": allowed, "response": response})


if __name__ == "__main__":
    raise SystemExit(main())
