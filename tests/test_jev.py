from __future__ import annotations

import copy
import io
import json
import os
import socket
import time
import urllib.error
from unittest import mock

from agent_decisions import gates, hook, jev, policy
from tests.helpers import FakeJev, StateTestCase, make_ctx

QUESTIONS = {
    "ok": {"type": "noul", "instructions": "Is it ok?"},
    "pick": {"type": "choice", "instructions": "Which?", "criteria": {"a": "A", "b": "B"}},
}


class AskTest(StateTestCase):
    def test_roundoff_at_existing_distribution_tolerance_is_accepted(self):
        for total in (0.99, 1.01):
            raw = json.dumps({"answers": {"pick": {
                "type": "choice", "choice": "a", "confidence": .65,
                "probabilities": {"a": .65, "b": total - .65}}}}).encode()
            with self.subTest(total=total):
                jev.ask({}, {"pick": QUESTIONS["pick"]}, key="k", post=FakeJev(raw=raw))

    def test_malformed_diagnostic_names_the_check_without_response_text(self):
        raw = b'{"answers":{"ok":{"type":"noul","noul":"private-response-text"}}}'
        with self.assertRaises(jev.JevError) as caught:
            jev.ask({}, {"ok": QUESTIONS["ok"]}, key="k", post=FakeJev(raw=raw))
        self.assertEqual(caught.exception.reason, "malformed")
        self.assertIn("ok: probability", str(caught.exception))
        self.assertNotIn("private-response-text", str(caught.exception))

    def test_numeric_fields_and_choice_distribution_are_validated(self):
        good = json.loads(FakeJev()(json.dumps({"questions": QUESTIONS}).encode(), "k", 1))
        cases = [
            ("ok", {"type": "noul", "noul": value}) for value in ("0.9", True, None, float("nan"))
        ]
        base = good["answers"]["pick"]
        cases += [("pick", {**base, "probabilities": value}) for value in (
            {"a": "0.9", "b": 0.1}, {"a": True, "b": 0.0}, {"a": 1.0},
            {"a": 1.0, "b": -0.1}, {"a": 0.8, "b": 0.8}, {"a": 0.1, "b": 0.9},
            {"a": 0.65, "b": 0.3399}, {"a": 0.65, "b": 0.3601},
        )]
        cases += [("pick", {**base, "confidence": value}) for value in ("0.9", True, 2.0)]
        for qid, answer in cases:
            with self.subTest(answer=answer):
                raw = json.dumps({"answers": {**good["answers"], qid: answer}}).encode()
                with self.assertRaises(jev.JevError) as caught:
                    jev.ask({}, QUESTIONS, key="k", post=FakeJev(raw=raw))
                self.assertEqual(caught.exception.reason, "malformed")

    def test_sends_model_state_questions_and_returns_answers(self):
        fake = FakeJev(ok=0.9, pick="b")
        answers = jev.ask({"x": 1}, QUESTIONS, key="k", post=fake)
        self.assertEqual(fake.requests, [{"model": policy.MODEL, "state": {"x": 1}, "questions": QUESTIONS}])
        self.assertEqual(answers["ok"]["noul"], 0.9)
        self.assertEqual(answers["pick"]["choice"], "b")

    def test_malformed_answers_raise(self):
        good = json.loads(FakeJev()(json.dumps({"questions": QUESTIONS}).encode(), "k", 1))
        bad = {
            "not json": b"<html>",
            "no answers": b"{}",
            "missing question": json.dumps({"answers": {"ok": good["answers"]["ok"]}}).encode(),
            "wrong type": json.dumps({"answers": {**good["answers"], "ok": good["answers"]["pick"]}}).encode(),
            "out of range": json.dumps({"answers": {**good["answers"], "ok": {"type": "noul", "noul": 1.5}}}).encode(),
            "choice outside criteria": json.dumps({"answers": {**good["answers"], "pick": {
                "type": "choice", "choice": "z", "probabilities": {"z": 1.0}}}}).encode(),
        }
        for name, raw in bad.items():
            with self.subTest(name), self.assertRaises(jev.JevError) as caught:
                jev.ask({}, QUESTIONS, key="k", post=FakeJev(raw=raw))
            self.assertEqual(caught.exception.reason, "malformed")


class KeyTest(StateTestCase):
    def test_env_then_file_then_no_key(self):
        self.assertEqual(jev.load_key(), "test-key-0123456789abcdef")
        os.environ.pop("TYPESAFE_API_KEY")
        key_file = self.dir / "key"
        key_file.write_text("file-key\n")
        with mock.patch.object(policy, "KEY_FILE", key_file):
            self.assertEqual(jev.load_key(), "file-key")
        with mock.patch.object(policy, "KEY_FILE", self.dir / "missing"), self.assertRaises(jev.JevError) as caught:
            jev.load_key()
        self.assertEqual(caught.exception.reason, "no_key")


class PostTest(StateTestCase):
    def test_reports_observed_context_limit_code_without_echoing_error_body(self):
        # Exact detail shape returned by the live oversized Jev 1.13 probe.
        error = urllib.error.HTTPError(policy.URL, 400, "x", {}, io.BytesIO(
            b'{"detail":{"error_type":"max_tokens_exceeded","state":"private-text"}}'))
        with mock.patch("urllib.request.urlopen", side_effect=error), self.assertRaises(jev.JevError) as caught:
            jev.post(b"{}", "k", 1)
        self.assertEqual(caught.exception.reason, "http_400")
        self.assertEqual(caught.exception.detail, "max_tokens_exceeded")
        self.assertNotIn("private-text", str(caught.exception))

    def test_unknown_validation_details_are_not_echoed(self):
        error = urllib.error.HTTPError(policy.URL, 400, "x", {}, io.BytesIO(
            b'{"detail":{"error_type":"private-error-text"}}'))
        self.assertEqual(self._raises(error), "http_400")

    def test_deadline_includes_reading_the_response_body(self):
        class SlowResponse(io.BytesIO):
            def read(self):
                time.sleep(0.25)
                return b'{}'

        start = time.monotonic()
        with mock.patch("urllib.request.urlopen", return_value=SlowResponse()), self.assertRaises(jev.JevError) as caught:
            jev.post(b'{}', "k", 0.05)
        self.assertEqual(caught.exception.reason, "timeout")
        self.assertLess(time.monotonic() - start, 0.2)

    def _raises(self, error):
        with mock.patch("urllib.request.urlopen", side_effect=error), self.assertRaises(jev.JevError) as caught:
            jev.post(b"{}", "k", 1.0)
        return caught.exception.reason

    def test_maps_transport_errors(self):
        http = lambda code: urllib.error.HTTPError(policy.URL, code, "x", {}, io.BytesIO(b""))
        self.assertEqual(self._raises(http(401)), "http_401")
        self.assertEqual(self._raises(http(429)), "http_429")
        self.assertEqual(self._raises(http(529)), "http_529")
        self.assertEqual(self._raises(TimeoutError()), "timeout")
        self.assertEqual(self._raises(socket.timeout()), "timeout")
        self.assertEqual(self._raises(urllib.error.URLError(TimeoutError())), "timeout")
        self.assertEqual(self._raises(urllib.error.URLError("refused")), "unreachable")

    def test_sends_bearer_key_and_never_puts_it_in_the_body(self):
        seen = {}

        class Response(io.BytesIO):
            def __enter__(self):
                return self

            def __exit__(self, *exc):
                return False

        def fake_urlopen(request, timeout):
            seen["auth"] = request.get_header("Authorization")
            seen["body"] = request.data
            seen["timeout"] = timeout
            return Response(b'{"answers": {}}')

        with mock.patch("urllib.request.urlopen", fake_urlopen):
            self.assertEqual(jev.post(b'{"state": 1}', "secret-key", 3.0), b'{"answers": {}}')
        self.assertEqual(seen, {"auth": "Bearer secret-key", "body": b'{"state": 1}', "timeout": 3.0})


class ContextLimitTest(StateTestCase):
    def test_peer_history_inputs_shrink_without_changing_current_dispatch_or_authority(self):
        ctx = self.context()
        ctx.evidence["user_messages"] = ["Preserve every required instruction.\n" * 1000]
        ctx.evidence["source_files"] = [{"path": "AGENTS.md", "text": "Required contract.\n" * 1000,
                                         "sha256": "rules-hash", "trust": "verified_file_observation"}]
        ctx.evidence["tool_calls_newest_first"] = [
            {"id": "peer-verdict", "tool": "mcp__codex_app__send_message_to_thread",
             "input": "Previously observed planning verdict.\n" * 260, "output": "Delivered",
             "trust": "unverified_tool_observation", "execution": {"cwd": "/other/worktree"}},
            *[{"id": f"dispatch-{i}", "tool": "followup_task", "input": "Earlier factual dispatch.\n" * 80,
               "output": "Completed", "trust": "unverified_tool_observation"} for i in range(7)]]
        pending = {"target": "v45_preserve_mac_evidence", "message": "Record the factual journal checkpoint."}
        state = {"pending_tool": "agentsfollowup_task", "pending_input": pending,
                 "decisions": [{"selected_approach": "Completed scoped decision.\n" * 400}],
                 "evidence": ctx.evidence}
        original = copy.deepcopy(state)
        requests = []
        fake = FakeJev()

        def post(body, key, timeout):
            requests.append(json.loads(body))
            if len(body) > 76000:
                raise jev.JevError("http_400", "max_tokens_exceeded")
            return fake(body, key, timeout)

        jev.ask(state, QUESTIONS, key="k", post=post)
        self.assertEqual(len(requests), 2)
        second = requests[-1]["state"]
        for field in ("pending_tool", "pending_input", "decisions"):
            self.assertEqual(second[field], original[field])
        for field in ("user_messages", "source_files", "brief", "contract_observations"):
            self.assertEqual(second["evidence"][field], original["evidence"][field])
        for before, after in zip(original["evidence"]["tool_calls_newest_first"],
                                 second["evidence"]["tool_calls_newest_first"]):
            self.assertEqual(after["id"], before["id"])
            self.assertEqual(after["trust"], before["trust"])
            self.assertEqual(after.get("execution"), before.get("execution"))
            self.assertTrue(after["input_truncated"])
            self.assertEqual(after["input_original_chars"], len(before["input"]))
            self.assertIn("characters cut", after["input"])
        self.assertEqual(ctx.evidence, original["evidence"])

    def context(self):
        ctx = make_ctx(host="codex")
        ctx.evidence.update({
            "user_messages": ["Preserve the guard.", "fix it asap"],
            "brief": "Keep the required review policy.",
            "tool_calls_newest_first": [{"id": "native-read", "tool": "exec_command",
                "input": "sed -n '1,400p' scripts/common.sh", "output": "diagnostic evidence\n" * 800,
                "trust": "unverified_tool_observation", "execution": {"cwd": "/other/worktree"}}],
            "source_files": [
                {"path": "scripts/common.sh", "text": "helper source\n" * 1000,
                 "sha256": "helper-hash", "trust": "verified_file_observation", "truncated": False},
                {"path": "AGENTS.md", "text": "Mandatory rules\n" * 500, "sha256": "rules-hash"},
                {"path": "contract.md", "text": "Mandatory contract\n" * 500, "sha256": "contract-hash"}],
            "contract_observations": {"contract.md": {"sha256": "contract-hash"}},
        })
        return ctx

    def transport(self, *, always_fail=False, pause=0):
        requests, timeouts = [], []
        fake = FakeJev()

        def post(body, key, timeout):
            requests.append(json.loads(body))
            timeouts.append(timeout)
            if len(requests) == 1 or always_fail:
                time.sleep(pause)
                raise jev.JevError("http_400", "max_tokens_exceeded")
            return fake(body, key, timeout)

        return post, requests, timeouts

    def test_retry_preserves_authority_and_records_actual_native_read_state(self):
        ctx = self.context()
        original = copy.deepcopy(ctx.evidence)
        post, requests, _ = self.transport()
        outcome = gates.edit_gate("mcp__codex_app__read_thread", {"threadId": "peer"},
                                  ctx, hook._asker(post))
        self.assertEqual(outcome.action, "allow")
        self.assertEqual(len(requests), 2)
        self.assertEqual(outcome.record["state"], requests[-1]["state"])
        self.assertEqual(ctx.evidence, original)
        first, second = (request["state"] for request in requests)
        self.assertEqual({k: v for k, v in first.items() if k != "evidence"},
                         {k: v for k, v in second.items() if k != "evidence"})
        for name in ("user_messages", "brief", "contract_observations"):
            self.assertEqual(second["evidence"][name], original[name])
        self.assertEqual(second["evidence"]["source_files"][1:], original["source_files"][1:])
        call = second["evidence"]["tool_calls_newest_first"][0]
        self.assertEqual({k: v for k, v in call.items() if k != "output"},
                         {k: v for k, v in original["tool_calls_newest_first"][0].items() if k != "output"})
        self.assertIn("characters cut", call["output"])
        self.assertTrue(second["evidence"]["source_files"][0]["truncated"])
        self.assertGreater(second["evidence"]["metadata"]["context_limit_chars_cut"], 0)

    def test_retry_uses_remaining_deadline(self):
        post, requests, timeouts = self.transport(pause=.02)
        metrics = {}
        jev.ask({"evidence": self.context().evidence}, QUESTIONS, key="k", post=post, metrics=metrics)
        self.assertEqual(len(requests), 2)
        self.assertLess(timeouts[1], timeouts[0] - .01)
        self.assertEqual(metrics["attempt_count"], 2)
        self.assertLess(metrics["request_bytes"], metrics["initial_request_bytes"])

    def test_retry_failure_still_denies_with_exact_last_attempt_state(self):
        post, requests, _ = self.transport(always_fail=True)
        outcome = gates.edit_gate("mcp__codex_app__wait_threads", {"timeoutMs": 0},
                                  self.context(), hook._asker(post))
        self.assertEqual(outcome.action, "deny")
        self.assertEqual(len(requests), 2)
        self.assertEqual(outcome.record["state"], requests[-1]["state"])
        self.assertIn("max_tokens_exceeded", outcome.message)
        self.assertIn("attempt_count=2", outcome.message)
        self.assertIn("user_messages=", outcome.message)
        self.assertNotIn("Mandatory rules", outcome.message)

    def test_peer_dispatch_recovers_when_the_old_first_reduction_is_still_too_large(self):
        ctx = self.context()
        original = copy.deepcopy(ctx.evidence)
        requests = []
        fake = FakeJev()

        def post(body, key, timeout):
            request = json.loads(body)
            requests.append(request)
            if len(body) > 27000:
                raise jev.JevError("http_400", "max_tokens_exceeded")
            return fake(body, key, timeout)

        pending = {"task_name": "v45_trim_failure_trace", "model": "gpt-6-luna",
                   "reasoning_effort": "max", "fork_turns": "none",
                   "message": "Read-only source trace. Do not edit files or execute remote commands."}
        outcome = gates.edit_gate("spawn_agent", pending, ctx, hook._asker(post))
        self.assertEqual(outcome.action, "allow", outcome.message)
        self.assertEqual(len(requests), 2)
        self.assertEqual(outcome.record["state"], requests[-1]["state"])
        self.assertEqual(requests[-1]["state"]["pending_input"], pending)
        self.assertEqual(ctx.evidence, original)
        self.assertEqual(requests[-1]["state"]["evidence"]["user_messages"], original["user_messages"])
        self.assertEqual(requests[-1]["state"]["evidence"]["source_files"][1:], original["source_files"][1:])

    def test_unrecognized_errors_and_unshortenable_state_are_not_retried(self):
        for error in (jev.JevError("http_400"), jev.JevError("unreachable"), jev.JevError("malformed"),
                      jev.JevError("http_400", "max_tokens_exceeded")):
            fake = FakeJev(raises=error)
            with self.subTest(error=str(error)), self.assertRaises(jev.JevError):
                jev.ask({"policy": "Keep every instruction."}, QUESTIONS, key="k", post=fake)
            self.assertEqual(len(fake.requests), 1)

    def test_concurrent_chunks_record_their_independent_review_states(self):
        ctx = self.context()
        original = copy.deepcopy(ctx.evidence)
        requests = []
        fake = FakeJev()

        def post(body, key, timeout):
            request = json.loads(body)
            requests.append(request)
            if "context_limit_chars_cut" not in request["state"]["evidence"]["metadata"]:
                raise jev.JevError("http_400", "max_tokens_exceeded")
            return fake(body, key, timeout)

        outcome = gates.edit_gate("Edit", {"file_path": "test.py", "old_string": "x" * 20000,
                                           "new_string": "y" * 20000}, ctx, hook._asker(post))
        self.assertEqual(outcome.action, "allow")
        self.assertEqual(ctx.evidence, original)
        reviewed = [r["state"] for r in requests
                    if "context_limit_chars_cut" in r["state"]["evidence"]["metadata"]]
        self.assertEqual(len(outcome.record["chunks"]), len(reviewed))
        for chunk in outcome.record["chunks"]:
            self.assertIn(chunk["state"], reviewed)
