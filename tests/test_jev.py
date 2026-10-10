from __future__ import annotations

import io
import json
import os
import socket
import time
import urllib.error
from unittest import mock

from agent_decisions import jev, policy
from tests.helpers import FakeJev, StateTestCase

QUESTIONS = {
    "ok": {"type": "noul", "instructions": "Is it ok?"},
    "pick": {"type": "choice", "instructions": "Which?", "criteria": {"a": "A", "b": "B"}},
}


class AskTest(StateTestCase):
    def test_numeric_fields_and_choice_distribution_are_validated(self):
        good = json.loads(FakeJev()(json.dumps({"questions": QUESTIONS}).encode(), "k", 1))
        cases = [
            ("ok", {"type": "noul", "noul": value}) for value in ("0.9", True, None, float("nan"))
        ]
        base = good["answers"]["pick"]
        cases += [("pick", {**base, "probabilities": value}) for value in (
            {"a": "0.9", "b": 0.1}, {"a": True, "b": 0.0}, {"a": 1.0},
            {"a": 1.0, "b": -0.1}, {"a": 0.8, "b": 0.8}, {"a": 0.1, "b": 0.9},
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
