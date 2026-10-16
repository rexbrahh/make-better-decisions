package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/ledger"
)

func TestLiveRoutineReadHosts(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_LIVE_HOSTS") != "1" {
		t.Skip("set AGENT_DECISIONS_LIVE_HOSTS=1 for native factual-read checks")
	}
	const marker = "factual-read-check-9b64f25d"
	for _, host := range []string{"codex", "claude"} {
		t.Run(host, func(t *testing.T) {
			repo := fixtureRepo(t, map[string]string{"marker.txt": marker + "\n"})
			state := t.TempDir()
			t.Setenv("AGENT_DECISIONS_STATE", state)
			ctx, cancel := context.WithTimeout(context.Background(), sessionLimit)
			defer cancel()
			prompt := "Read marker.txt and report its exact contents. Make no edits."
			var cmd *exec.Cmd
			if host == "codex" {
				cmd = exec.CommandContext(ctx, "codex", "-p", "cli", "exec", "--model", "gpt-6-luna",
					"-c", `model_reasoning_effort="max"`, "--sandbox", "read-only", "-C", repo, "--json", prompt)
			} else {
				cmd = exec.CommandContext(ctx, "claude", "-p", prompt, "--model", claudeModel,
					"--max-budget-usd", claudeBudget, "--output-format", "json", "--permission-mode", "acceptEdits")
			}
			cmd.Dir = repo
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil || !strings.Contains(string(out), marker) {
				t.Fatalf("native %s read failed: %v; stderr: %s", host, err, stderr.String())
			}
			if strings.Contains(stderr.String(), "Command blocked by PreToolUse hook") ||
				strings.Contains(strings.ToLower(stderr.String()), "hook returned invalid") ||
				strings.Contains(stderr.String(), "--profile requires embedded mode") {
				t.Fatalf("native factual-read diagnostic: %s", stderr.String())
			}
			if host == "claude" {
				var result struct {
					IsError bool `json:"is_error"`
				}
				if err := json.Unmarshal(out, &result); err != nil || result.IsError {
					t.Fatalf("Claude result: %v, is_error=%v", err, result.IsError)
				}
			} else if !strings.Contains(string(out), `"type":"turn.completed"`) {
				t.Fatal("native Codex turn did not complete")
			}
			paths, err := filepath.Glob(filepath.Join(state, "sessions", "*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			prompted := false
			for _, path := range paths {
				id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
				rows, err := ledger.Entries(id, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					prompted = prompted || row.Value("gate") == "user_prompt"
					if row.Value("gate") == "decide" || row.Value("gate") == "edit" {
						t.Fatalf("factual read entered %s review", row.Value("gate"))
					}
				}
			}
			if !prompted {
				t.Fatal("the native hook did not capture the read request")
			}
			t.Logf("%s read the fixture with active hooks and no consultation", host)
		})
	}
}
