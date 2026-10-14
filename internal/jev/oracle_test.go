package jev

import (
	"bufio"
	"encoding/hex"
	stdjson "encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// oracleScript answers one JSON request per stdin line with one json.dumps line on stdout. Text arrives as
// hex of its UTF-8 bytes with surrogatepass; a JSON value arrives as hex of its json.dumps text; raw bytes
// arrive as hex. policy.KEY_FILE always points at a test file, never at the real key.
const oracleScript = `
import json, math, os, pathlib, sys
from unittest import mock
sys.path.insert(0, sys.argv[1])
from agent_decisions import jev, policy

def text(wire):
    return bytes.fromhex(wire).decode("utf-8", "surrogatepass")

def value(wire):
    return json.loads(text(wire))

def outcome(call):
    try:
        return ["ok", call()]
    except Exception as error:
        return [type(error).__name__, str(error)]

def scripted(steps):
    bodies = []
    def post(body, key, timeout):
        bodies.append(body.hex())
        step = steps[len(bodies) - 1]
        if step[0] == "raise":
            raise jev.JevError(step[1], step[2])
        return bytes.fromhex(step[1])
    return post, bodies

def load_key(env, data, path):
    path = pathlib.Path(path)
    if data is None:
        path.unlink(missing_ok=True)
    else:
        path.write_bytes(bytes.fromhex(data))
    if env is None:
        os.environ.pop("TYPESAFE_API_KEY", None)
    else:
        os.environ["TYPESAFE_API_KEY"] = text(env)
    with mock.patch.object(policy, "KEY_FILE", path):
        return outcome(jev.load_key)

def post(url, body, key, timeout):
    with mock.patch.object(policy, "URL", url):
        reply = outcome(lambda: jev.post(bytes.fromhex(body), text(key), timeout))
    if reply[0] == "ok":
        reply[1] = reply[1].hex()
    return reply

for line in sys.stdin:
    op, *a = json.loads(line)
    if op == "ask":
        state, questions = value(a[0]), value(a[1])
        transport, bodies = scripted(a[2])
        metrics = {}
        result = outcome(lambda: jev.ask(state, questions, key="k", post=transport, metrics=metrics))
        reply = [result, bodies, {k: v for k, v in metrics.items() if not k.endswith("_ms")}, state]
    elif op == "check":
        reply = outcome(lambda: jev._check(value(a[0]), value(a[1])))
    elif op == "fsum":
        reply = outcome(lambda: math.fsum(a[0]))
    elif op == "latin1":
        reply = outcome(lambda: text(a[0]).encode("latin-1").hex())
    elif op == "context_limit":
        reply = outcome(lambda: str(jev._context_limit_error(value(a[0]), value(a[1]), a[2], a[3])))
    elif op == "load_key":
        reply = load_key(a[0], a[1], a[2])
    elif op == "post":
        reply = post(a[0], a[1], a[2], a[3])
    print(json.dumps(reply), flush=True)
`

type oracleCase struct {
	name    string
	request []any
	run     func() any // the reply that json.dumps must render as Python's line
}

// compareWithPython runs every case through one CPython process named by AGENT_DECISIONS_PYTHON and
// compares each reply with json.dumps of the Go reply. Python answers every case before Go runs any.
func compareWithPython(t *testing.T, cases []oracleCase) {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var input strings.Builder
	for _, c := range cases {
		line, err := stdjson.Marshal(c.request)
		if err != nil {
			t.Fatal(err)
		}
		input.Write(line)
		input.WriteByte('\n')
	}
	command := exec.Command(python, "-I", "-c", oracleScript, repoRoot)
	command.Stderr = os.Stderr
	command.Stdin = strings.NewReader(input.String())
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(strings.NewReader(string(output)))
	lines.Buffer(nil, 1<<30)
	i := 0
	for ; lines.Scan(); i++ {
		if i >= len(cases) {
			t.Fatalf("python answered more than %d requests", len(cases))
		}
		got := dumps(t, cases[i].run())
		if want := lines.Text(); got != want {
			t.Errorf("%s:\n  go     %s\n  python %s", cases[i].name, around(got, want), around(want, got))
		}
	}
	if i != len(cases) {
		t.Fatalf("python answered %d of %d requests", i, len(cases))
	}
}

// around returns s near the first byte where it differs from other.
func around(s, other string) string {
	i := 0
	for i < len(s) && i < len(other) && s[i] == other[i] {
		i++
	}
	return fmt.Sprintf("[%d] %q", i, s[max(0, i-60):min(len(s), i+60)])
}

func outcome(value any, err error) []any {
	if err == nil {
		return []any{"ok", value}
	}
	if typed, ok := err.(interface{ PyType() string }); ok {
		return []any{typed.PyType(), err.Error()}
	}
	return []any{fmt.Sprintf("Go %T", err), err.Error()}
}

func wire(s string) string { return hex.EncodeToString([]byte(s)) }

// step is one scripted transport reply: raw response bytes, or a raised JevError.
type step struct {
	raw            string
	reason, detail string // reason "" means return raw
}

func respondWith(raw string) step      { return step{raw: raw} }
func raise(reason, detail string) step { return step{reason: reason, detail: detail} }
func contextLimit() step               { return raise("http_400", "max_tokens_exceeded") }
func (s step) wire() []any {
	if s.reason == "" {
		return []any{"raw", hex.EncodeToString([]byte(s.raw))}
	}
	var detail any
	if s.detail != "" {
		detail = s.detail
	}
	return []any{"raise", s.reason, detail}
}

func askCase(t *testing.T, name, state, questions string, steps ...step) oracleCase {
	var script []any
	for _, s := range steps {
		script = append(script, s.wire())
	}
	return oracleCase{
		name:    "ask " + name,
		request: []any{"ask", wire(state), wire(questions), script},
		run: func() any {
			stateValue := loads(t, state).(*json.Object)
			var bodies []any
			post := func(body []byte, key string, timeout time.Duration) ([]byte, error) {
				bodies = append(bodies, hex.EncodeToString(body))
				s := steps[len(bodies)-1]
				if s.reason != "" {
					return nil, &JevError{Reason: s.reason, Detail: s.detail}
				}
				return []byte(s.raw), nil
			}
			metrics := &json.Object{}
			answers, err := Ask(stateValue, loads(t, questions).(*json.Object), "k", post, metrics)
			var result []any
			if err == nil {
				result = outcome(answers, nil)
			} else {
				result = outcome(nil, err)
			}
			kept := &json.Object{}
			for _, key := range metrics.Keys() {
				if !strings.HasSuffix(key, "_ms") {
					kept.Set(key, metrics.Value(key))
				}
			}
			if bodies == nil {
				bodies = []any{}
			}
			return []any{result, bodies, kept, stateValue}
		},
	}
}

const twoQuestions = `{"ok": {"type": "noul", "instructions": "Is it ok?"},
	"pick": {"type": "choice", "instructions": "Which?", "criteria": {"a": "A", "b": "B"}}}`

func TestAskMatchesPython(t *testing.T) {
	setUp(t)
	good := `{"model": "jev-1.13.0", "answers": {"ok": {"type": "noul", "noul": 0.25},
		"pick": {"type": "choice", "choice": "b", "confidence": 0.5, "probabilities": {"a": 0.4, "b": 0.6}}},
		"usage": {"input_tokens": 120, "flag": true, "neg": -1, "nan": NaN, "inf": Infinity, "f": 1.5,
		"text": "7", "big": 1` + strings.Repeat("0", 30) + `}}`
	bigUsage := `{"answers": {"ok": {"type": "noul", "noul": 1}}, "usage": {"x": 1` + strings.Repeat("0", 400) + `}}`
	longModel := `{"model": "` + strings.Repeat("\u00e9\u4e2d", 80) + `", "answers": {"ok": {"type": "noul", "noul": 0}}}`
	ok := `{"ok": {"type": "noul"}}`
	evidenceState := func(extra string) string {
		history := `[{"id": "c1", "tool": "Bash", "input": "` + strings.Repeat("ls -la; ", 900) +
			`", "output": "` + strings.Repeat("file.txt ", 900) + `", "trust": "unverified_tool_observation"}]`
		return `{` + extra + `"evidence": {"user_messages": ["fix it"], "brief": null,
			"tool_calls_newest_first": ` + history + `, "source_files": [], "metadata": {}},
			"pending_input": {"command": "rm -rf build"}}`
	}
	labels := []string{"", "a b", "dotted.name-1_x", strings.Repeat("q", 64), strings.Repeat("q", 65), "caf\u00e9", "\U0001f600"}
	cases := []oracleCase{
		askCase(t, "good", `{"x": 1}`, twoQuestions, respondWith(good)),
		askCase(t, "big usage int", `{}`, ok, respondWith(bigUsage)),
		askCase(t, "long model", `{}`, ok, respondWith(longModel)),
		askCase(t, "not json", `{}`, ok, respondWith("<html>")),
		askCase(t, "list", `{}`, ok, respondWith("[1]")),
		askCase(t, "answers list", `{}`, ok, respondWith(`{"answers": []}`)),
		askCase(t, "invalid utf-8", `{}`, ok, respondWith("{\"answers\": {\"ok\": \xff}}")),
		askCase(t, "utf-8 bom", `{}`, ok, respondWith("\xef\xbb\xbf"+`{"answers": {"ok": {"type": "noul", "noul": 0.5}}}`)),
		askCase(t, "utf-16", `{}`, ok, respondWith(utf16LE(`{"answers": {"ok": {"type": "noul", "noul": 0.5}}}`))),
		askCase(t, "lone surrogate", `{}`, ok, respondWith("{\"answers\": {\"ok\": {\"type\": \"noul\", \"noul\": 0.5, \"x\": \"\xed\xa0\x80\"}}}")),
		askCase(t, "deep", `{}`, ok, respondWith(strings.Repeat("[", 3000)+strings.Repeat("]", 3000))),
		askCase(t, "timeout", `{}`, ok, raise("timeout", "")),
		askCase(t, "http_400 without detail", `{}`, ok, raise("http_400", "")),
		askCase(t, "unshortenable limit", `{"policy": "Keep every instruction."}`, ok, contextLimit()),
		askCase(t, "retry succeeds", evidenceState(""), twoQuestions, contextLimit(), respondWith(good)),
		askCase(t, "retry fails", evidenceState(""), twoQuestions, contextLimit(), contextLimit()),
		askCase(t, "retry other error", evidenceState(""), twoQuestions, contextLimit(), raise("unreachable", "")),
		askCase(t, "retry with decision", evidenceState(`"decision": "Bash ls", "contracts": {"AGENTS.md": "x"}, `),
			twoQuestions, contextLimit(), respondWith(good)),
		askCase(t, "retry with list decision", evidenceState(`"decision": ["a", 1], "contracts": ["AGENTS.md", 2], `),
			twoQuestions, contextLimit(), respondWith(good)),
		askCase(t, "evidence not a dict", `{"evidence": [1]}`, ok, contextLimit()),
	}
	for _, label := range labels {
		questions := dumps(t, json.NewObject(label, json.NewObject("type", "noul")))
		answers := dumps(t, json.NewObject("answers", json.NewObject(label, json.NewObject("type", "noul", "noul", 2))))
		cases = append(cases, askCase(t, "label "+label, `{}`, questions, respondWith(answers)))
	}
	compareWithPython(t, cases)
}

// utf16LE encodes ASCII text as UTF-16 little-endian without a byte order mark.
func utf16LE(ascii string) string {
	var b strings.Builder
	for i := 0; i < len(ascii); i++ {
		b.WriteByte(ascii[i])
		b.WriteByte(0)
	}
	return b.String()
}

func TestCheckMatchesPython(t *testing.T) {
	noul := `{"type": "noul"}`
	choice := `{"type": "choice", "criteria": {"a": "A", "b": "B"}}`
	big := "1" + strings.Repeat("0", 400)
	answers := []struct{ answer, question string }{
		{`{"type": "noul", "noul": 0}`, noul},
		{`{"type": "noul", "noul": 1}`, noul},
		{`{"type": "noul", "noul": 2}`, noul},
		{`{"type": "noul", "noul": -0.0}`, noul},
		{`{"type": "noul", "noul": 1.0000000001}`, noul},
		{`{"type": "noul", "noul": false}`, noul},
		{`{"type": "noul", "noul": Infinity}`, noul},
		{`{"type": "noul", "noul": ` + big + `}`, noul},
		{`{"type": "noul"}`, noul},
		{`{"noul": 0.5}`, noul},
		{`[]`, noul},
		{`"noul"`, noul},
		{`7`, noul},
		{`null`, noul},
		{`{"type": ["noul"], "noul": 0.5}`, noul},
		{`{"type": "noul", "noul": 0.5}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"b": 0, "a": 1}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 0.5, "b": 0.5}}`, choice},
		{`{"type": "choice", "choice": "b", "confidence": 1, "probabilities": {"a": 0.5, "b": 0.5}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 0.66, "b": 0.33}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 0.66, "b": 0.329}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 0.7, "b": 0.31}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 0.7, "b": 0.311}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 1, "b": ` + big + `}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": ` + big + `, "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 1}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": 1, "b": 0, "c": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": [1, 0]}`, choice},
		{`{"type": "choice", "choice": ["a"], "confidence": 1, "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": {"a": 1}, "confidence": 1, "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": 1, "confidence": 1, "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": null, "confidence": 1, "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1}`, choice},
		{`{"type": "choice", "confidence": 1, "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": "1", "probabilities": {"a": 1, "b": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": true, "b": 0}}`, choice},
		{`{"type": "choice", "choice": "a", "confidence": 1, "probabilities": {"a": NaN, "b": 0}}`, choice},
	}
	var cases []oracleCase
	for _, c := range answers {
		cases = append(cases, oracleCase{
			name:    "check " + c.answer + " " + c.question,
			request: []any{"check", wire(c.answer), wire(c.question)},
			run: func() any {
				answer, err := json.Loads(c.answer)
				if err != nil {
					t.Fatal(err)
				}
				question, _ := json.Loads(c.question)
				return outcome(nil, check(answer, question))
			},
		})
	}
	compareWithPython(t, cases)
}

func TestFsumMatchesPython(t *testing.T) {
	vectors := [][]float64{
		{0.1, 0.2, 0.3},
		{0.65, 0.33999999999999997},
		{1, 0x1p-53, 0x1p-106},
		{-1, -0x1p-53, -0x1p-106},
		{1, 0x1p-53, -0x1p-106},
		{0x1p-1074, 0x1p-1074, 1},
		{},
	}
	seed := uint64(0x9e3779b97f4a7c15)
	for range 200 {
		var vector []float64
		for range 1 + seed%9 {
			seed ^= seed << 13
			seed ^= seed >> 7
			seed ^= seed << 17
			vector = append(vector, math.Ldexp(float64(int64(seed>>11)-1<<52), int(seed%80)-100))
		}
		vectors = append(vectors, vector)
	}
	var cases []oracleCase
	for _, vector := range vectors {
		values := make([]any, len(vector))
		for i, v := range vector {
			values[i] = v
		}
		cases = append(cases, oracleCase{
			name:    fmt.Sprint("fsum ", vector),
			request: []any{"fsum", values},
			run:     func() any { return outcome(fsum(vector), nil) },
		})
	}
	compareWithPython(t, cases)
}

func TestEncodingErrorsMatchPython(t *testing.T) {
	var cases []oracleCase
	for _, key := range []string{"plain", "caf\u00e9\u00ff", "k\u4e2dx", "\U0001f600\u4e2d", "a\u0100b\u0101",
		"x\xed\xa0\x80y", "\u00e9\U0001f600\u00e9\u4e2d"} {
		text := "Bearer " + key
		cases = append(cases, oracleCase{
			name:    "latin1 " + key,
			request: []any{"latin1", wire(text)},
			run: func() any {
				encoded, err := latin1(text)
				return outcome(hex.EncodeToString([]byte(encoded)), err)
			},
		})
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	files := []*string{nil, ptr(""), ptr(" \n"), ptr("file-key\n"), ptr("a\r\nb\rc\n"), ptr("\xffkey"),
		ptr("key\xc3"), ptr("ke\xe2\x82y"), ptr("\xed\xa0\x80"), ptr("\xf0\x9f\x98"), ptr("\xf4\x90\x80\x80"),
		ptr("\xc0\x80"), ptr("\xe0\x80\x80"), ptr("\xf0\x80\x80\x80x"), ptr("\u00a0key\u3000")}
	for _, env := range []*string{nil, ptr(""), ptr("  "), ptr(" env-key ")} {
		for _, data := range files {
			name := fmt.Sprintf("load_key env=%v file=%q", env != nil, deref(data))
			var dataWire any
			if data != nil {
				dataWire = hex.EncodeToString([]byte(*data))
			}
			var envWire any
			if env != nil {
				envWire = wire(*env)
			}
			cases = append(cases, oracleCase{
				name:    name,
				request: []any{"load_key", envWire, dataWire, path},
				run: func() any {
					if data == nil {
						os.Remove(path)
					} else if err := os.WriteFile(path, []byte(*data), 0o600); err != nil {
						t.Fatal(err)
					}
					if env == nil {
						t.Setenv("TYPESAFE_API_KEY", "")
						os.Unsetenv("TYPESAFE_API_KEY")
					} else {
						t.Setenv("TYPESAFE_API_KEY", *env)
					}
					patchKeyFile(t, path)
					return outcome(LoadKey())
				},
			})
		}
	}
	compareWithPython(t, cases)
}

func ptr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "<missing>"
	}
	return *s
}

func TestContextLimitErrorMatchesPython(t *testing.T) {
	states := []string{
		`{}`,
		`{"evidence": {"user_messages": ["caf\u00e9"], "brief": null, "source_files": [{"path": "a"}]},
			"decisions": [{"x": 1}], "pending_input": {"command": "\u4e2d"}}`,
		`{"evidence": []}`,
		`{"evidence": "text"}`,
		`{"evidence": {}, "decisions": NaN}`,
	}
	var cases []oracleCase
	for _, state := range states {
		for _, questions := range []string{`{}`, twoQuestions} {
			cases = append(cases, oracleCase{
				name:    "context_limit " + state + questions,
				request: []any{"context_limit", wire(state), wire(questions), 2, 12345},
				run: func() any {
					err := contextLimitError(loads(t, state).(*json.Object), loads(t, questions).(*json.Object), 2, 12345)
					if _, ok := err.(*JevError); ok {
						return outcome(err.Error(), nil)
					}
					return outcome(nil, err)
				},
			})
		}
	}
	compareWithPython(t, cases)
}

// TestPostMatchesPython sends the same requests from urllib and from post to one local server, which
// answers each by the body's mode and echoes the request line and headers on success.
func TestPostMatchesPython(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local port binding is unavailable here: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(serveMode)}}
	server.Start()
	defer server.Close()
	modes := []string{"ok", "empty", "400-limit", "400-other", "400-text", "400-list", "400-bom",
		"400-utf16", "400-deep", "400-big", "401", "429", "500", "302", "303", "307", "308", "loop", "slow",
		"slow-body", "204"}
	var cases []oracleCase
	for _, mode := range modes {
		for _, key := range []string{"secret-key", "caf\u00e9", "k\u4e2d"} {
			body := `{"mode": "` + mode + `"}`
			timeout := 1.0
			cases = append(cases, oracleCase{
				name:    "post " + mode + " " + key,
				request: []any{"post", server.URL + "/v1/answers", hex.EncodeToString([]byte(body)), wire(key), timeout},
				run: func() any {
					original := url
					url = server.URL + "/v1/answers"
					defer func() { url = original }()
					raw, err := Post([]byte(body), key, time.Duration(timeout*float64(time.Second)))
					return outcome(hex.EncodeToString(raw), err)
				},
			})
		}
	}
	compareWithPython(t, cases)
}

// serveMode answers a request by the "mode" in its JSON body.
func serveMode(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body struct{ Mode string }
	stdjson.Unmarshal(data, &body)
	mode := body.Mode
	if mode == "" {
		mode = r.URL.Query().Get("mode") // a redirected GET has no body
	}
	switch mode {
	case "ok", "empty":
		if mode == "empty" {
			return
		}
		echo(w, r, data)
	case "400-limit":
		http.Error(w, `{"detail":{"error_type":"max_tokens_exceeded","state":"private-text"}}`, 400)
	case "400-other":
		http.Error(w, `{"detail":{"error_type":"private-error-text"}}`, 400)
	case "400-text":
		http.Error(w, "<html>bad</html>", 400)
	case "400-list":
		http.Error(w, `[{"detail": 1}]`, 400)
	case "400-bom":
		http.Error(w, "\xef\xbb\xbf"+`{"detail":{"error_type":"max_tokens_exceeded"}}`, 400)
	case "400-utf16":
		http.Error(w, utf16LE(`{"detail":{"error_type":"max_tokens_exceeded"}}`), 400)
	case "400-deep":
		http.Error(w, strings.Repeat("[", 4000), 400)
	case "400-big":
		http.Error(w, `{"detail":{"error_type":"max_tokens_exceeded"},"pad":"`+strings.Repeat("x", 5000)+`"}`, 400)
	case "401", "429", "500":
		var code int
		fmt.Sscan(mode, &code)
		http.Error(w, "no", code)
	case "302", "303", "307", "308":
		var code int
		fmt.Sscan(mode, &code)
		http.Redirect(w, r, "/redirected?mode=ok", code)
	case "loop":
		http.Redirect(w, r, "/loop?mode=loop", 302)
	case "slow":
		time.Sleep(1500 * time.Millisecond)
	case "slow-body":
		w.Header().Set("Content-Length", "10")
		w.Write([]byte("{"))
		w.(http.Flusher).Flush()
		time.Sleep(1500 * time.Millisecond)
	case "204":
		w.WriteHeader(204)
	}
}

// echo writes the request line and the headers both clients send, so a reply shows what reached the server.
func echo(w http.ResponseWriter, r *http.Request, data []byte) {
	var names []string
	for name := range r.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		lines = append(lines, name+": "+strings.Join(r.Header[name], ","))
	}
	fmt.Fprintf(w, "%s %s %t\n%s\n%s", r.Method, r.URL.Path, r.Host != "", strings.Join(lines, "\n"), data)
}
