package policy

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/oracletest"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// oracleScript prints the policy values that Go must reproduce, with HOME and AGENT_DECISIONS_STATE
// taken from the environment of the process.
const oracleScript = `
import json, sys
sys.path.insert(0, sys.argv[1])
from agent_decisions import policy
print(json.dumps([str(policy.state_dir()), str(policy.KEY_FILE), policy.MODEL, policy.REVIEW_CONTRACT, policy.URL,
    policy.TIMEOUT_S, policy.USER_MESSAGES, policy.EVIDENCE_CHARS, policy.CLIP_CHARS, policy.FINAL_MESSAGE_CHARS,
    policy.REQUEST_CHARS, policy.MIN_FACT_CHARS, policy.REFUSE_AT, policy.BIND_AT, policy.STOP_AT, policy.FOLLOW_AT,
    policy.REFUSAL_CAP, policy.RETAIN_DAYS, policy.CACHED_REVIEWS,
    [sorted(tools) for tools in (policy.SHELL_TOOLS, policy.EDIT_TOOLS, policy.READ_TOOLS, policy.QUESTION_TOOLS)],
    policy.POLICY], ensure_ascii=True))
`

func sortedNames(tools map[string]bool) []any {
	var names []string
	for name := range tools {
		names = append(names, name)
	}
	slices.Sort(names) // ASCII names: byte order is code point order
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}

// TestOracleValues compares StateDir, KeyFile and every constant with policy.py for several HOME and
// AGENT_DECISIONS_STATE spellings.
func TestOracleValues(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot := oracletest.Source(t)
	for _, env := range [][2]string{
		{"/Users/x", ""}, {"/Users/x/", ""}, {"/", ""}, {"//", ""}, {"/h", "/tmp/state"}, {"/h", "/tmp//a/./b/"},
		{"/h", "//net/state"}, {"/h", "rel/../state"}, {"/h", "."}, {"/h", "\u00e9t\u00e9"},
	} {
		t.Setenv("HOME", env[0])
		t.Setenv("AGENT_DECISIONS_STATE", env[1])
		command := exec.Command(python, "-I", "-c", oracleScript, repoRoot)
		command.Stderr = os.Stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("python oracle: %v", err)
		}
		stateDir, err := StateDir()
		if err != nil {
			t.Fatal(err)
		}
		keyFile, err := KeyFile()
		if err != nil {
			t.Fatal(err)
		}
		got := []any{stateDir, keyFile, Model, ReviewContract, URL, TimeoutS, UserMessages, EvidenceChars,
			ClipChars, FinalMessageChars, RequestChars, MinFactChars, RefuseAt, BindAt, StopAt, FollowAt, RefusalCap,
			RetainDays, CachedReviews,
			[]any{sortedNames(ShellTools), sortedNames(EditTools), sortedNames(ReadTools), sortedNames(QuestionTools)},
			Policy}
		g, err := json.Dumps(got, json.Options{})
		if w := strings.TrimSpace(string(output)); err != nil || g != w {
			t.Errorf("HOME=%q AGENT_DECISIONS_STATE=%q:\n go %s\n py %s", env[0], env[1], g, w)
		}
	}
}
