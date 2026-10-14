package argparse

import (
	"bytes"
	stdjson "encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Python's argparse suggests the closest choice through difflib.get_close_matches.
func TestCloseMatch(t *testing.T) {
	for _, c := range []struct{ word, want string }{
		{"exce", "exec"}, {"exe", "exec"}, {"clade", "claude"}, {"codec", "codex"}, {"c", ""}, {"xyz", ""},
		{"cod", "codex"}, {"claud", "claude"},
	} {
		if got := closeMatch(c.word, []string{"claude", "codex", "exec"}); got != c.want {
			t.Errorf("closeMatch(%q) = %q, want %q", c.word, got, c.want)
		}
	}
}

// TestSplitLinesMatchesPython compares the help wrapping with HelpFormatter._split_lines on hyphenated,
// long and spaced words at small widths.
func TestSplitLinesMatchesPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	texts := []string{
		"Owner-authorized review exception for one canonical session and its native workers.",
		"Revoke an owner-authorized session review exception.",
		"ab-c x-y-z ab-cd a-b-c-d ab-c-de well-known-fact -abc abc- a--b over--long",
		"  spaced\tout \n words  ", "averyveryverylongwordwithoutanyhyphens and-a-very-long-hyphenated-word",
		"show this help message and exit",
		"x.--y 1--2 a_b-cd --abc a- - a---b co-op's re-run! 3-in-1 _a-_b ab-c-d-e-fgh x,--y? q--",
	}
	type wrapCase struct {
		Text  string `json:"text"`
		Width int    `json:"width"`
	}
	var cases []wrapCase
	for _, text := range texts {
		for width := 1; width <= 40; width++ {
			cases = append(cases, wrapCase{text, width})
		}
	}
	input, _ := stdjson.Marshal(cases)
	script := `import argparse, json, sys
f = argparse.HelpFormatter("prog")
print(json.dumps([f._split_lines(c["text"], c["width"]) for c in json.load(sys.stdin)]))`
	command := exec.Command(python, "-I", "-c", script)
	command.Stdin = strings.NewReader(string(input))
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var want [][]string
	if err := stdjson.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		got := splitLines(c.Text, c.Width)
		if !slices.Equal(got, want[i]) && !(len(got) == 0 && len(want[i]) == 0) {
			t.Errorf("splitLines(%q, %d) = %q, want %q", c.Text, c.Width, got, want[i])
		}
	}
}

// hookParser is the parser of agent_decisions/hook.py main.
func hookParser(stdout, stderr *bytes.Buffer, ignoreEnvironment bool) *Parser {
	return &Parser{Prog: "agent-decisions hook", Stdout: stdout, Stderr: stderr, IgnoreEnvironment: ignoreEnvironment,
		Actions: []*Action{
			HelpAction(),
			{Kind: Store, Dest: "event", Choices: []string{"pre_tool_use", "stop", "subagent_stop", "session_start", "user_prompt_submit"}},
			{Kind: Store, Options: []string{"--host"}, Dest: "host", Required: true, Choices: []string{"claude", "codex"}},
		}}
}

// colorVariables are the environment variables that the help colors and width read.
var colorVariables = []string{"COLUMNS", "NO_COLOR", "FORCE_COLOR", "TERM", "PYTHON_COLORS"}

// TestHookParserMatchesPython runs the parser of hook.py main on argument permutations, missing, extra and
// invalid arguments, abbreviations, help, widths and colors, under python3 -I and without -I.
func TestHookParserMatchesPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	argvs := [][]string{
		{"session_start", "--host", "claude"}, {"--host", "claude", "session_start"}, {"--host=codex", "stop"},
		{"--ho", "codex", "stop"}, {"--h", "codex", "stop"}, {"stop"}, {"--host", "claude"}, {},
		{"sesion_start", "--host", "claude"}, {"stop", "--host", "claud"}, {"stop", "extra", "--host", "codex"},
		{"stop", "--host", "codex", "extra"}, {"--", "stop", "--host", "codex"}, {"--host", "codex", "--", "stop"},
		{"--host", "codex", "stop", "--"}, {"--host", "codex", "--", "--"}, {"stop", "--host"}, {"stop", "-x"},
		{"-h"}, {"stop", "--help"}, {"--host", "codex", "-h", "bogus"}, {"-5", "--host", "codex"},
		{"--host", "codex", "stop", "stop"}, {"--host", "a b"}, {"stop", "--host", "codex", "--host", "claude"},
		{"--", "--host", "codex"}, {"user_prompt_submit", "--host", "\xed\xa0\x80"},
	}
	environments := []map[string]string{
		{}, {"COLUMNS": "40"}, {"COLUMNS": "20"}, {"COLUMNS": "200"}, {"COLUMNS": "x"},
		{"FORCE_COLOR": "1"}, {"FORCE_COLOR": "1", "NO_COLOR": "1"}, {"FORCE_COLOR": "1", "TERM": "dumb"},
		{"PYTHON_COLORS": "1"}, {"PYTHON_COLORS": "0", "FORCE_COLOR": "1"}, {"FORCE_COLOR": "1", "COLUMNS": "30"},
	}
	type oracleCase struct {
		Argv  []string          `json:"-"`
		Codes [][]rune          `json:"argv"`
		Env   map[string]string `json:"env"`
	}
	var cases []oracleCase
	for _, environment := range environments {
		for _, argv := range argvs {
			codes := [][]rune{}
			for _, arg := range argv {
				points := []rune{}
				for rest := arg; rest != ""; {
					r, size := str.DecodeRune(rest)
					points, rest = append(points, r), rest[size:]
				}
				codes = append(codes, points)
			}
			cases = append(cases, oracleCase{argv, codes, environment})
		}
	}
	input, err := stdjson.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	// Arguments cross as lists of code points, so a lone surrogate survives the JSON.
	script := `import argparse, contextlib, io, json, os, sys
names = ` + "[" + `"COLUMNS", "NO_COLOR", "FORCE_COLOR", "TERM", "PYTHON_COLORS"]
out = []
for case in json.load(sys.stdin):
    for name in names:
        os.environ.pop(name, None)
    os.environ.update(case["env"])
    parser = argparse.ArgumentParser(prog="agent-decisions hook")
    parser.add_argument("event", choices=("pre_tool_use", "stop", "subagent_stop", "session_start", "user_prompt_submit"))
    parser.add_argument("--host", required=True, choices=("claude", "codex"))
    stdout, stderr = io.StringIO(), io.StringIO()
    status, namespace = None, None
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        try:
            argv = ["".join(map(chr, codes)) for codes in case["argv"]]
            namespace = sorted(vars(parser.parse_args(argv)).items())
        except SystemExit as exit:
            status = exit.code
    encode = lambda s: s.encode("utf-8", "backslashreplace").decode("utf-8")
    out.append([status, namespace, encode(stdout.getvalue()), encode(stderr.getvalue())])
print(json.dumps(out))`
	for _, isolated := range []bool{true, false} {
		arguments := []string{"-c", script}
		if isolated {
			arguments = append([]string{"-I"}, arguments...)
		}
		command := exec.Command(python, arguments...)
		command.Stdin = bytes.NewReader(input)
		output, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		var want [][4]any
		if err := stdjson.Unmarshal(output, &want); err != nil {
			t.Fatal(err)
		}
		for _, name := range colorVariables {
			t.Setenv(name, "")
		}
		for i, c := range cases {
			for _, name := range colorVariables {
				if value, ok := c.Env[name]; ok {
					os.Setenv(name, value)
				} else {
					os.Unsetenv(name)
				}
			}
			var stdout, stderr bytes.Buffer
			ns, err := hookParser(&stdout, &stderr, isolated).ParseArgs(c.Argv)
			var status, namespace any
			if err != nil {
				status = float64(err.(*Exit).Code)
			} else {
				namespace = []any{[]any{"event", ns["event"]}, []any{"host", ns["host"]}}
			}
			got := [4]any{status, namespace, stdout.String(), stderr.String()}
			gotJSON, _ := stdjson.Marshal(got)
			wantJSON, _ := stdjson.Marshal(want[i])
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("-I=%v argv %q env %v:\nGo     %s\nPython %s", isolated, c.Argv, c.Env, gotJSON, wantJSON)
			}
		}
	}
}
