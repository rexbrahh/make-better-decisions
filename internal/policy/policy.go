// Package policy holds every tunable value and the ownership policy, in one place. Tune from the ledger
// with Rex.
//
// The policy functions that read the ledger (installation_policy, review_mode, session_review_exception,
// fail_open) live in package ledger, because Go forbids the import cycle that policy.py breaks with a
// lazy import.
//
// py: agent_decisions/policy.py
package policy

import (
	"os"

	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

// py: agent_decisions/policy.py MODEL, REVIEW_CONTRACT, URL, TIMEOUT_S
const (
	Model          = "jev-1.13.0"
	ReviewContract = "scoped-review-2"
	URL            = "https://api.typesafe.ai/v1/systemone"
	TimeoutS       = 3.0
)

// KeyFile returns the path of the Typesafe API key. Python computes it once at import; this reads HOME at
// call time, which gives the same path for the life of one hook process.
//
// py: agent_decisions/policy.py KEY_FILE
func KeyFile() (string, error) {
	home, err := home()
	if err != nil {
		return "", err
	}
	return pathlib.Join(home, ".config/sops-nix/secrets/typesafe_api_key"), nil
}

// py: agent_decisions/policy.py USER_MESSAGES ... CACHED_REVIEWS
const (
	UserMessages      = 5
	EvidenceChars     = 80_000 // 20k tokens at about 4 characters per token
	ClipChars         = 16_000 // 4k tokens; a longer text keeps its head and tail
	FinalMessageChars = 16_000
	RequestChars      = 16_000
	MinFactChars      = 12 // shorter facts match almost anything

	RefuseAt      = 0.6
	BindAt        = 0.8
	StopAt        = 0.7
	FollowAt      = 0.5
	RefusalCap    = 2
	RetainDays    = 14
	CachedReviews = 16
)

// Tool name sets. Treat them as read-only, like the Python frozensets.
//
// py: agent_decisions/policy.py SHELL_TOOLS, EDIT_TOOLS, READ_TOOLS, QUESTION_TOOLS
var (
	ShellTools = map[string]bool{"Bash": true, "exec_command": true, "shell": true, "local_shell": true}
	EditTools  = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
		"apply_patch": true, "mcp__filesystem__write_file": true, "mcp__filesystem__edit_file": true}
	ReadTools     = map[string]bool{"Read": true, "Grep": true, "Glob": true}
	QuestionTools = map[string]bool{"AskUserQuestion": true, "request_user_input": true,
		"request_user_input_async": true}
)

// py: agent_decisions/policy.py POLICY
const Policy = "The user decides product behaviour and anything destructive, irreversible, " +
	"external, or paid. The agent decides engineering details and anything that " +
	"the user's words or the evidence already settle. A later specific user instruction " +
	"supersedes any earlier Jev verdict, including a binding pick. User instructions take precedence."

// StateDir returns the private state directory: AGENT_DECISIONS_STATE when set and non-empty, else
// ~/.local/state/agent-decisions. The result is spelled as str(Path(...)) spells it.
//
// py: agent_decisions/policy.py state_dir
func StateDir() (string, error) {
	if override := os.Getenv("AGENT_DECISIONS_STATE"); override != "" {
		return pathlib.Str(override), nil
	}
	home, err := home()
	if err != nil {
		return "", err
	}
	return pathlib.Join(home, ".local/state/agent-decisions"), nil
}

// home returns str(Path.home()), or the RuntimeError that Path.home() raises.
//
// py: Lib/pathlib/_local.py Path.home (Python 3.15)
func home() (string, error) {
	dir, err := ospath.ExpandUser("~")
	if err != nil {
		return "", err
	}
	if dir == "~" {
		return "", &pyerr.Error{Class: "RuntimeError", Msg: "Could not determine home directory."}
	}
	return pathlib.Str(dir), nil
}
