package argparse

import (
	"bytes"
	stdjson "encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/oracletest"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

func TestPlainHelpPreservesDescriptionsAndSubcommands(t *testing.T) {
	t.Setenv("COLUMNS", "20")
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("NO_COLOR", "")
	t.Setenv("PYTHON_COLORS", "1")
	const description = "Explain the complete decision boundary for a native session and its workers without splitting this description across terminal lines."
	var stdout, stderr bytes.Buffer
	p := &Parser{Prog: "tool", Stdout: &stdout, Stderr: &stderr, Actions: []*Action{
		HelpAction(),
		{Kind: Store, Options: []string{"--details"}, Dest: "details", Help: description},
		{Kind: Parsers, Dest: "mode", Choices: []string{"exec"}, Parser: &Parser{Prog: "tool exec"}, ChoiceHelp: "run the signed command"},
	}}
	_, err := p.ParseArgs([]string{"--help"})
	if exit, ok := err.(*Exit); !ok || exit.Code != 0 {
		t.Fatalf("help returned %v", err)
	}
	text := stdout.String()
	for _, content := range []string{"--details DETAILS", description, "exec", "run the signed command"} {
		if !strings.Contains(text, content) {
			t.Errorf("help lost %q", content)
		}
	}
	if strings.Contains(text, "\x1b") || stderr.Len() != 0 {
		t.Fatalf("help is not plain stdout: %q, stderr %q", text, stderr.String())
	}
}

func TestInvalidChoiceRejectsWithoutSuggestion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, err := hookParser(&stdout, &stderr).ParseArgs([]string{"stop", "--host", "clade"})
	if exit, ok := err.(*Exit); !ok || exit.Code != 2 {
		t.Fatalf("invalid choice returned %v", err)
	}
	if !strings.Contains(stderr.String(), "invalid choice: 'clade' (choose from 'claude', 'codex')") {
		t.Fatalf("invalid-choice diagnostic %q", stderr.String())
	}
}

// hookParser is the parser of agent_decisions/hook.py main.
func hookParser(stdout, stderr *bytes.Buffer) *Parser {
	return &Parser{Prog: "agent-decisions hook", Stdout: stdout, Stderr: stderr,
		Actions: []*Action{
			HelpAction(),
			{Kind: Store, Dest: "event", Choices: []string{"pre_tool_use", "stop", "subagent_stop", "session_start", "user_prompt_submit"}},
			{Kind: Store, Options: []string{"--host"}, Dest: "host", Required: true, Choices: []string{"claude", "codex"}},
		}}
}

// colorVariables are the environment variables that the help colors and width read.
var colorVariables = []string{"COLUMNS", "NO_COLOR", "FORCE_COLOR", "TERM", "PYTHON_COLORS"}

// TestHookParserMatchesPython runs the parser of hook.py main on argument permutations, missing, extra and
// invalid arguments and abbreviations under python3 -I and without -I. Retired CLI presentation is normalized.
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
			ns, err := hookParser(&stdout, &stderr).ParseArgs(c.Argv)
			var status, namespace any
			if err != nil {
				status = float64(err.(*Exit).Code)
			} else {
				namespace = []any{[]any{"event", ns["event"]}, []any{"host", ns["host"]}}
			}
			got := [4]any{status, namespace, oracletest.CLIPresentation(stdout.String()), oracletest.CLIPresentation(stderr.String())}
			want[i][2] = oracletest.CLIPresentation(want[i][2].(string))
			want[i][3] = oracletest.CLIPresentation(want[i][3].(string))
			gotJSON, _ := stdjson.Marshal(got)
			wantJSON, _ := stdjson.Marshal(want[i])
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("-I=%v argv %q env %v:\nGo     %s\nPython %s", isolated, c.Argv, c.Env, gotJSON, wantJSON)
			}
		}
	}
}
