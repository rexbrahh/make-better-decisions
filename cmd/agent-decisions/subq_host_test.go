package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/install"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	pyjson "github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/subq"
)

// TestLiveSnapHosts uses the installed hooks with temporary signed policy on both native hosts.
func TestLiveSnapHosts(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_SUBQ_HOSTS") != "1" {
		t.Skip("set AGENT_DECISIONS_SUBQ_HOSTS=1 for paid native Snap qualification")
	}
	if _, err := subq.LoadKey(); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(home, ".local/share/agent-decisions/current")
	decide := filepath.Join(runtime, "bin/ts-decide")
	request := `{"decision":"Which encoding preserves the required names?","options":[{"id":"ascii","description":"ASCII encoding"},{"id":"utf8","description":"UTF-8 encoding"}]}`
	prompt := "Native plugin qualification with synthetic fixture data. Preserve names such as Zoë and 李 exactly. " +
		"Read codec.txt first. Then run the following exact consultation once as a separate complete shell call:\n" +
		decide + " <<'EOF'\n" + request + "\nEOF\n" +
		"Preserve the actual verdict. Do not retry or implement an encoding choice. " +
		"Independently, perform this exact authorized edit: in greet.py replace the string \"Hello, \" with \"Hi, \". " +
		"Use the declared edit tool (Edit on Claude, apply_patch on Codex). Make no other change. " +
		"Do not read protected state or keys, change policy, delegate or contact peers."
	for _, host := range []string{"codex", "claude"} {
		t.Run(host, func(t *testing.T) {
			repo := fixtureRepo(t, map[string]string{"greet.py": greetSource,
				"codec.txt": "ASCII cannot encode 李. UTF-8 encodes both Zoë and 李.\n"})
			state := t.TempDir()
			t.Setenv("AGENT_DECISIONS_STATE", state)
			entry, err := install.InstallationEntry(host, state, pyjson.NewObject(), []any{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			entry.Value("spec").(*pyjson.Object).Set("review_provider", "subq")
			if err := ledger.Append(entry); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), sessionLimit)
			defer cancel()
			var cmd *exec.Cmd
			if host == "codex" {
				cmd = exec.CommandContext(ctx, "codex", "-p", "cli", "exec", "--model", "gpt-6-luna",
					"-c", `model_reasoning_effort="max"`, "--sandbox", "workspace-write", "-C", repo, "--json", prompt)
			} else {
				cmd = exec.CommandContext(ctx, "claude", "-p", prompt, "--model", claudeModel,
					"--max-budget-usd", claudeBudget, "--output-format", "json", "--permission-mode", "acceptEdits")
			}
			cmd.Dir = repo
			var stderr strings.Builder
			cmd.Stderr = &stderr
			started := time.Now()
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("native %s failed: %v; stderr: %s", host, err, stderr.String())
			}
			if strings.Contains(strings.ToLower(stderr.String()), "hook returned invalid") ||
				strings.Contains(stderr.String(), "--profile requires embedded mode") {
				t.Fatalf("native hook diagnostic: %s", stderr.String())
			}
			if host == "claude" {
				var result struct {
					IsError bool `json:"is_error"`
				}
				if err := json.Unmarshal(out, &result); err != nil || result.IsError {
					t.Fatalf("native Claude result failed: %v, is_error=%v", err, result.IsError)
				}
			} else if !strings.Contains(string(out), `"type":"turn.completed"`) {
				t.Fatal("native Codex turn did not complete")
			}
			paths, err := filepath.Glob(filepath.Join(state, "sessions", "*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			consulted, edited, calls := false, false, 0
			for _, path := range paths {
				id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
				if id == "installation" {
					continue
				}
				rows, err := ledger.Entries(id, nil)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					metrics, _ := row.Value("metrics").(*pyjson.Object)
					if metrics == nil {
						continue
					}
					apiCalls, _ := metrics.Value("api_calls").([]any)
					for _, value := range apiCalls {
						call := value.(*pyjson.Object)
						if call.Value("provider") != "subq" || call.Value("returned_model") != policy.SubQModel {
							t.Fatal("native review used a different provider or unverified model")
						}
						calls++
						if row.Value("gate") == "decide" {
							identity, _ := row.Value("review_identity").(*pyjson.Object)
							consulted = row.Value("action") == "advisory" && row.Value("pick") == "utf8" &&
								row.Value("receipt_id") != nil && identity != nil && identity.Value("provider") == "subq" &&
								call.Value("batch_count") == pyjson.Int("2")
						}
					}
					if row.Value("gate") == "edit" && row.Value("action") == "allow" && len(apiCalls) > 0 {
						edited = true
					}
				}
			}
			data, err := os.ReadFile(filepath.Join(repo, "greet.py"))
			if err != nil || string(data) != strings.Replace(greetSource, "Hello, ", "Hi, ", 1) || !consulted || !edited {
				t.Fatalf("native Snap qualification: read=%v, consulted=%v, edited=%v, calls=%d", err, consulted, edited, calls)
			}
			t.Logf("%s: Snap receipt, two consultation batches and exact edit verified; %d review calls, %.0fs", host, calls, time.Since(started).Seconds())
		})
	}
}
