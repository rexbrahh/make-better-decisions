package main

// Live end-to-end checks of the installed plugin through real headless Claude Code sessions. They spend
// model budget and run only with AGENT_DECISIONS_LIVE_HOSTS=1, outside any sandbox, with CLAUDE_CONFIG_DIR
// naming the config dir where the plugin is installed:
//
//	AGENT_DECISIONS_LIVE_HOSTS=1 go test -v -count=1 -timeout 30m -run LiveHost ./cmd/agent-decisions
//
// Each scenario runs one `claude -p` session in a temp git repo with AGENT_DECISIONS_STATE set to a temp
// dir, so the hook of the installed runtime writes its ledger there. The model is free to consult, edit or
// stop as it likes: each check judges what the hook did with whatever the model did, from the ledger rows
// and the session's JSON result, never from the model's prose.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	claudeModel  = "haiku" // the cheapest model measured to follow the deciding skill
	claudeBudget = "0.15"  // USD per session; a measured session costs about $0.01
	sessionLimit = 5 * time.Minute
)

// ledgerRow is the part of a hook ledger entry that the checks read.
type ledgerRow struct {
	Gate      string `json:"gate"`
	Action    string `json:"action"`
	ReceiptID string `json:"receipt_id"`
	Pick      string `json:"pick"`
	State     struct {
		PendingTool  string          `json:"pending_tool"`
		PendingInput json.RawMessage `json:"pending_input"`
		Decisions    []struct {
			ReceiptID string `json:"receipt_id"`
		} `json:"decisions"`
	} `json:"state"`
	Answers struct {
		NeedsDecision struct{ Noul float64 } `json:"needs_decision"`
		Covered       struct{ Noul float64 } `json:"covered"`
	} `json:"answers"`
}

// edits returns the edit gate rows whose target is file: the file_path of a file tool, or a whole path
// element of a shell command. The content a tool writes does not count, so a check_prices.py that names
// prices.py in its text does not match.
func edits(rows []ledgerRow, file string) []ledgerRow {
	name := regexp.MustCompile(`(^|[^\w.-])` + regexp.QuoteMeta(file) + `\b`)
	var out []ledgerRow
	for _, row := range rows {
		var input struct {
			FilePath string `json:"file_path"`
			Command  string `json:"command"`
		}
		if row.Gate != "edit" {
			continue
		}
		// An input that is not an object is kept, so that the checks see it instead of skipping it.
		if json.Unmarshal(row.State.PendingInput, &input) != nil || filepath.Base(input.FilePath) == file ||
			name.MatchString(input.Command) {
			out = append(out, row)
		}
	}
	return out
}

// hasReceipt reports whether row was judged against a decision whose receipt a ts-decide row minted.
func hasReceipt(row ledgerRow, rows []ledgerRow) bool {
	for _, decision := range row.State.Decisions {
		for _, decide := range rows {
			if decide.Gate == "decide" && decision.ReceiptID != "" && decide.ReceiptID == decision.ReceiptID {
				return true
			}
		}
	}
	return false
}

// session is the outcome of one headless session.
type session struct {
	repo   string
	rows   []ledgerRow
	result struct {
		Subtype           string  `json:"subtype"`
		IsError           bool    `json:"is_error"`
		TotalCostUSD      float64 `json:"total_cost_usd"`
		NumTurns          int     `json:"num_turns"`
		PermissionDenials []struct {
			ToolName string `json:"tool_name"`
		} `json:"permission_denials"`
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=live", "-c", "user.email=live@example.invalid",
		"-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// fixtureRepo returns a temp git repo with files committed.
func fixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	for name, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "fixture")
	return repo
}

func nativeCodexTrustOverride(t *testing.T, repo string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("projects={%q={trust_level=\"trusted\"}}", canonical)
}

// runClaude runs one headless Claude Code session on a fixture repo and returns its result and ledger.
func runClaude(t *testing.T, files map[string]string, prompt string) session {
	t.Helper()
	s := session{repo: fixtureRepo(t, files)}
	state := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), sessionLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--model", claudeModel, "--max-budget-usd", claudeBudget,
		"--output-format", "json", "--permission-mode", "acceptEdits")
	cmd.Dir = s.repo
	cmd.Env = append(os.Environ(), "AGENT_DECISIONS_STATE="+state)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	started := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(started)
	if jsonErr := json.Unmarshal(out, &s.result); jsonErr != nil {
		t.Fatalf("claude -p: %v, result not JSON (%v)\nstdout: %s\nstderr: %s", err, jsonErr, out, stderr.String())
	}
	t.Logf("session: $%.4f, %d turns, %.0fs, %s, permission denials %v", s.result.TotalCostUSD,
		s.result.NumTurns, elapsed.Seconds(), s.result.Subtype, s.result.PermissionDenials)
	if err != nil || s.result.IsError {
		t.Errorf("claude -p ended in error: %v, subtype %s\nstderr: %s", err, s.result.Subtype, stderr.String())
	}

	paths, err := filepath.Glob(filepath.Join(state, "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	prompted := false
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var signed struct{ Entry ledgerRow }
			if err := json.Unmarshal([]byte(line), &signed); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			row := signed.Entry
			s.rows = append(s.rows, row)
			prompted = prompted || row.Gate == "user_prompt"
			switch row.Gate {
			case "decide":
				t.Logf("ledger: decide %s pick=%q receipt=%q", row.Action, row.Pick, row.ReceiptID)
			case "edit":
				t.Logf("ledger: edit %s %s needs_decision=%.2f covered=%.2f receipts=%d", row.Action,
					row.State.PendingTool, row.Answers.NeedsDecision.Noul, row.Answers.Covered.Noul, len(row.State.Decisions))
			case "user_prompt", "user_context":
			default:
				t.Logf("ledger: %s %s", row.Gate, row.Action)
			}
		}
	}
	if !prompted {
		t.Fatalf("the installed hook wrote no user_prompt row to %s: the session did not load the plugin", state)
	}
	return s
}

func noDenials(t *testing.T, s session) {
	t.Helper()
	for _, row := range s.rows {
		if row.Action == "deny" {
			t.Errorf("hook denied a %s call (%s)", row.Gate, row.State.PendingTool)
		}
	}
}

const greetSource = "def greet(name):\n    return \"Hello, \" + name\n"

const pricesSource = `import json
import urllib.request


def fetch_price(symbol):
    """Return the latest price of symbol from the quote service."""
    with urllib.request.urlopen("https://quotes.example.com/v1/" + symbol) as response:
        return json.load(response)["price"]
`

func TestLiveHostSessions(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_LIVE_HOSTS") != "1" {
		t.Skip("set AGENT_DECISIONS_LIVE_HOSTS=1 to run real headless host sessions")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatal(err)
	}

	// An exact instruction settles the edit, so the hook allows it without a consultation.
	t.Run("exact_edit", func(t *testing.T) {
		s := runClaude(t, map[string]string{"greet.py": greetSource},
			`In greet.py, replace the string "Hello, " with "Hi, ". Use the Edit tool. Make no other change.`)
		noDenials(t, s)
		if len(edits(s.rows, "greet.py")) == 0 {
			t.Errorf("no edit of greet.py reached the hook")
		}
		data, err := os.ReadFile(filepath.Join(s.repo, "greet.py"))
		if want := strings.Replace(greetSource, "Hello, ", "Hi, ", 1); err != nil || string(data) != want {
			t.Errorf("greet.py is %q (%v), want %q", data, err, want)
		}
	})

	// The caching strategy is a material choice the user left open. Every edit that the hook allowed on
	// prices.py must carry a ts-decide receipt; any other edit must be denied. prices.py must not change
	// through a path the hook did not review.
	t.Run("design_choice", func(t *testing.T) {
		s := runClaude(t, map[string]string{"prices.py": pricesSource},
			"fetch_price in prices.py calls the quote service too often. Add caching to fetch_price. "+
				"The caching strategy is not settled: choose between functools.lru_cache and a dict cache with a "+
				"60-second time-to-live, then implement your choice in prices.py with the Edit tool.")
		allowed, denied := 0, 0
		for _, row := range edits(s.rows, "prices.py") {
			switch {
			case row.Action == "deny":
				denied++
			case hasReceipt(row, s.rows):
				allowed++
			default:
				t.Errorf("hook allowed an edit of prices.py without a receipt: needs_decision=%.2f covered=%.2f",
					row.Answers.NeedsDecision.Noul, row.Answers.Covered.Noul)
			}
		}
		t.Logf("prices.py edits: %d allowed with a receipt, %d denied", allowed, denied)
		data, err := os.ReadFile(filepath.Join(s.repo, "prices.py"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != pricesSource && allowed == 0 {
			t.Errorf("prices.py changed, but the hook allowed no reviewed edit of it")
		}
	})

	// A question changes nothing, so the hook reviews no edit and denies nothing.
	t.Run("read_only", func(t *testing.T) {
		s := runClaude(t, map[string]string{"greet.py": greetSource},
			`What string does greet("Bo") in greet.py return? Read the file and answer in one line. Do not change any file.`)
		noDenials(t, s)
		for _, row := range s.rows {
			if row.Gate == "edit" || row.Gate == "plan" {
				t.Errorf("hook reviewed a %s call (%s) in a read-only session", row.Gate, row.State.PendingTool)
			}
		}
		if status := git(t, s.repo, "status", "--porcelain"); status != "" {
			t.Errorf("the repo changed:\n%s", status)
		}
	})
}
