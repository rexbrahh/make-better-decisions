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

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/install"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	pyjson "github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/subq"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// TestLiveSnapHosts uses the installed hooks with temporary signed policy on both native hosts.
func TestLiveSnapHosts(t *testing.T) {
	if os.Getenv("AGENT_DECISIONS_SUBQ_HOSTS") != "1" {
		t.Skip("set AGENT_DECISIONS_SUBQ_HOSTS=1 for paid native Snap qualification")
	}
	key, err := subq.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	provider, _ := policy.Provider("subq")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(home, ".local/share/agent-decisions/current")
	decide := filepath.Join(runtime, "bin/ts-decide")
	request := `{"decision":"Which encoding preserves the required names?","options":[{"id":"ascii","description":"ASCII encoding"},{"id":"utf8","description":"UTF-8 encoding"}],"scope":{"paths":["codec.py"],"invariants":["Preserve the required names exactly."]},"contracts":["codec.txt"]}`
	prompt := "Native plugin qualification with synthetic fixture data. Preserve names such as Zoë and 李 exactly. " +
		"Read codec.txt first. Then run the following exact consultation once as a separate complete shell call:\n" +
		decide + " <<'EOF'\n" + request + "\nEOF\n" +
		"Preserve the actual verdict. Do not retry. If the verdict is ADVISORY or BINDING, implement its selected " +
		"encoding in codec.py. Keep that file to one ENCODING assignment containing the selected Python codec name. " +
		"For any other verdict, leave codec.py unchanged. " +
		"Independently, perform this exact authorized edit: in greet.py replace the string \"Hello, \" with \"Hi, \". " +
		"Use the declared edit tool (Edit on Claude, apply_patch on Codex), separately for each file. Make no other change. " +
		"Do not read protected state or keys, change policy, delegate or contact peers."
	for _, mode := range []string{"codex", "claude", "codex-worker"} {
		t.Run(mode, func(t *testing.T) {
			host := strings.TrimSuffix(mode, "-worker")
			worker := mode == "codex-worker"
			files := map[string]string{"greet.py": greetSource, "codec.py": "ENCODING = None\n",
				".gitignore": "__pycache__/\n",
				"codec.txt":  "ASCII cannot encode 李. UTF-8 encodes both Zoë and 李.\n"}
			runPrompt := prompt
			if worker {
				files[".codex/agents/snap-luna-probe.toml"] = `name = "snap_luna_probe"
description = "Implement the selected encoding from one scoped parent receipt."
model = "gpt-6-luna"
model_reasoning_effort = "max"
developer_instructions = "This task is self-contained. Read only codec.txt, codec.py and host-required skills. Use exactly one declared apply_patch call to set ENCODING to the parent's picked codec. Stop on denial. Do not read memory, unrelated files, protected state or keys. Do not consult again, use shell writes, delegate or change policy."
`
				runPrompt = "This native plugin qualification is self-contained and uses synthetic data. Read only codec.txt, codec.py, greet.py and host-required skills. Do not read memory or unrelated files. " +
					"Preserve Zoë and 李 exactly. Read codec.txt. Run this exact consultation once as a separate complete shell call:\n" +
					decide + " <<'EOF'\n" + request + "\nEOF\n" +
					"Keep the actual verdict. Do not retry. If it is ADVISORY or BINDING with a receipt, spawn exactly one native child. " +
					"Set task_name to ad_ followed by that exact receipt ID. Set agent_type to snap_luna_probe, model to gpt-6-luna, reasoning_effort to max and fork_turns to none. " +
					"Include Decision receipt: <actual receipt> and Accepted option: <actual pick> as separate lines in its message. " +
					"Tell it to read codec.txt and codec.py and set the one ENCODING assignment to the picked Python codec using exactly one declared apply_patch call. " +
					"It must stop on denial without a retry. It must not consult again, read memory or unrelated files, use shell writes, delegate, inspect keys or protected state, or change policy. " +
					"Wait for that same child to finish. The parent must change no file before the child completes. " +
					"Do not edit codec.py yourself. For other verdicts, leave codec.py unchanged and do not spawn. " +
					"After the child completes, make this exact authorized parent edit with apply_patch: replace the string \"Hello, \" with \"Hi, \" in greet.py. " +
					"Make no other change. Do not inspect protected state or keys or change policy."
			}
			repo := fixtureRepo(t, files)
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
					"-c", `model_reasoning_effort="max"`, "-c", nativeCodexTrustOverride(t, repo),
					"--sandbox", "workspace-write", "-C", repo, "--json", runPrompt)
			} else {
				cmd = exec.CommandContext(ctx, "claude", "-p", prompt, "--model", claudeModel,
					"--max-budget-usd", claudeBudget, "--output-format", "json", "--permission-mode", "acceptEdits")
			}
			cmd.Dir = repo
			var stderr strings.Builder
			cmd.Stderr = &stderr
			started := time.Now()
			out, err := cmd.Output()
			stderrText := evidence.Redact(stderr.String(), []string{key})
			if err != nil {
				t.Fatalf("native %s failed: %v; stdout: %s; stderr: %s", host, err, evidence.Redact(string(out), []string{key}), stderrText)
			}
			if strings.Contains(strings.ToLower(stderrText), "hook returned invalid") ||
				strings.Contains(stderrText, "--profile requires embedded mode") {
				t.Fatalf("native hook diagnostic: %s", stderrText)
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
			consulted, edited, coveredEdit, calls, consultations := false, false, false, 0, 0
			receipts := map[string]bool{}
			workerTranscripts := map[string]string{}
			parentTranscripts := map[string]string{}
			var editRows []*pyjson.Object
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
					if worker && row.Value("gate") == "host_identity" {
						agent, _ := row.Value("agent_id").(string)
						path, _ := row.Value("transcript_path").(string)
						if row.Value("agent_id") == nil {
							thread, _ := row.Value("thread_id").(string)
							parentTranscripts[thread] = path
						} else {
							workerTranscripts[agent] = path
						}
					}
					if row.Value("gate") == "decide" {
						consultations++
					}
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
							state, _ := row.Value("state").(*pyjson.Object)
							if state == nil {
								t.Fatal("consultation has no recorded state")
							}
							scope, _ := state.Value("scope").(*pyjson.Object)
							if scope == nil {
								t.Fatal("consultation has no recorded scope")
							}
							paths, _ := scope.Value("paths").([]any)
							invariants, _ := scope.Value("invariants").([]any)
							if len(paths) != 1 || paths[0] != "codec.py" || len(invariants) != 1 || invariants[0] != "Preserve the required names exactly." {
								t.Fatal("consultation scope differs from the required fixture scope")
							}
							identity, _ := row.Value("review_identity").(*pyjson.Object)
							consulted = (row.Value("action") == "advisory" || row.Value("action") == "binding") && row.Value("pick") == "utf8" &&
								row.Value("receipt_id") != nil && identity != nil && identity.Value("provider") == "subq" &&
								identity.Value("model") == provider.Model && identity.Value("context_profile") == provider.ContextProfile &&
								identity.Value("review_contract") == policy.ReviewContract &&
								call.Value("batch_count") == pyjson.Int("2")
							if receipt, ok := row.Value("receipt_id").(string); consulted && ok && receipt != "" {
								receipts[receipt] = true
							}
						}
					}
					if row.Value("gate") == "edit" && row.Value("action") == "allow" && len(apiCalls) > 0 {
						editRows = append(editRows, row)
					}
				}
			}
			for _, row := range editRows {
				state, _ := row.Value("state").(*pyjson.Object)
				if state == nil {
					t.Fatal("allowed edit has no recorded state")
				}
				input, _ := state.Value("pending_input").(*pyjson.Object)
				tool, _ := state.Value("pending_tool").(string)
				if input != nil {
					t.Logf("%s edit input: tool=%q keys=%v", host, tool, input.Keys())
				}
				edited = edited || snapEditTargets(tool, input, repo, "greet.py")
				if !snapEditTargets(tool, input, repo, "codec.py") {
					continue
				}
				answers, _ := row.Value("answers").(*pyjson.Object)
				if answers == nil {
					t.Fatal("reviewed encoding edit has no answers")
				}
				covered, _ := answers.Value("covered").(*pyjson.Object)
				if covered == nil {
					t.Fatal("reviewed encoding edit has no coverage answer")
				}
				score, _ := covered.Value("noul").(float64)
				if covered.Value("noul") == pyjson.Int("1") {
					score = 1
				}
				decisions, _ := state.Value("decisions").([]any)
				if worker {
					encoded, err := pyjson.Dumps(decisions, pyjson.Options{})
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("native worker encoding edit: agent=%v decisions=%s", row.Value("agent_id"), encoded)
				}
				coverageAt := policy.BindAt
				if row.Value("agent_id") == nil {
					coverageAt = policy.FollowAt
				}
				receiptMatched := false
				for _, value := range decisions {
					decision, _ := value.(*pyjson.Object)
					if decision != nil {
						receipt, _ := decision.Value("receipt_id").(string)
						matched := receipts[receipt]
						if worker {
							matched = matched && row.Value("agent_id") == "/root/ad_"+receipt &&
								decision.Value("origin") == "parent_dispatch" && decision.Value("action") == "binding" &&
								decision.Value("pick") == "utf8"
						}
						receiptMatched = receiptMatched || matched
					}
				}
				t.Logf("%s encoding edit: receipt_matched=%v covered=%.3f threshold=%.2f", host, receiptMatched, score, coverageAt)
				coveredEdit = coveredEdit || (receiptMatched && score >= coverageAt)
			}
			data, err := os.ReadFile(filepath.Join(repo, "greet.py"))
			if err != nil || string(data) != strings.Replace(greetSource, "Hello, ", "Hi, ", 1) || !consulted || !edited || !coveredEdit || consultations != 1 {
				t.Fatalf("native Snap qualification: read=%v, consulted=%v, exact_edit=%v, covered_edit=%v, consultations=%d, calls=%d", err, consulted, edited, coveredEdit, consultations, calls)
			}
			check := exec.Command("python3", "-c", `import ast, codecs, pathlib, sys
tree = ast.parse(pathlib.Path(sys.argv[1]).read_text())
assert len(tree.body) == 1
assignment = tree.body[0]
assert isinstance(assignment, ast.Assign) and len(assignment.targets) == 1
assert isinstance(assignment.targets[0], ast.Name) and assignment.targets[0].id == "ENCODING"
encoding = ast.literal_eval(assignment.value)
assert isinstance(encoding, str) and codecs.lookup(encoding).name == "utf-8"
for name in ("Zoë", "李"):
    assert name.encode(encoding).decode(encoding) == name
`, filepath.Join(repo, "codec.py"))
			if out, err := check.CombinedOutput(); err != nil {
				t.Fatalf("encoding acceptance check: %v\n%s", err, out)
			}
			if changed := git(t, repo, "status", "--porcelain"); changed != " M codec.py\n M greet.py\n" {
				t.Fatalf("unexpected changed fixture files: %q", changed)
			}
			if worker {
				if len(workerTranscripts) != 1 || len(parentTranscripts) != 1 {
					t.Fatalf("got %d native workers and %d parents, want one each", len(workerTranscripts), len(parentTranscripts))
				}
				var parentPath string
				for _, path := range parentTranscripts {
					parentPath = path
				}
				for agent, path := range workerTranscripts {
					verifySnapNativeWorker(t, path, agent, parentPath)
				}
			}
			t.Logf("%s: Snap receipt, two consultation batches, covered encoding edit and exact edit verified; %d review calls, %.0fs", host, calls, time.Since(started).Seconds())
		})
	}
}

func verifySnapNativeWorker(t *testing.T, path, agent, parentPath string) {
	t.Helper()
	parent, err := transcript.Load(parentPath, "codex")
	if err != nil || parent.Identity == nil || parent.Identity.AgentID != nil {
		t.Fatalf("native parent identity failed: %v", err)
	}
	observed, err := transcript.Load(path, "codex")
	if err != nil || observed.Identity == nil || observed.Identity.AgentID == nil ||
		*observed.Identity.AgentID != agent || observed.Identity.ParentThreadID == nil ||
		*observed.Identity.ParentThreadID != parent.Identity.ThreadID ||
		observed.Identity.SessionID != parent.Identity.SessionID || observed.Brief != nil {
		t.Fatalf("opaque native worker identity failed: %v", err)
	}
	dispatches := 0
	for _, call := range parent.ToolCalls {
		if call.Name != "spawn_agent" || call.Output == "" {
			continue
		}
		var input, output struct {
			TaskName string `json:"task_name"`
		}
		if json.Unmarshal([]byte(call.Input), &input) == nil && json.Unmarshal([]byte(call.Output), &output) == nil &&
			input.TaskName == strings.TrimPrefix(agent, "/root/") && output.TaskName == agent {
			dispatches++
		}
	}
	if dispatches != 1 {
		t.Fatalf("got %d matching completed parent dispatches, want one", dispatches)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	turns, completed := 0, false
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type, Model, Effort string
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row.Type == "turn_context" {
			turns++
			if row.Payload.Model != "gpt-6-luna" || row.Payload.Effort != "max" {
				t.Fatalf("native worker used %s/%s", row.Payload.Model, row.Payload.Effort)
			}
		}
		completed = completed || row.Type == "event_msg" && row.Payload.Type == "task_complete"
	}
	if turns == 0 || !completed {
		t.Fatal("native worker has no verified model context or terminal completion")
	}
	t.Logf("opaque native worker %s, thread %s, model gpt-6-luna/max, completed", agent, observed.Identity.ThreadID)
}

func snapEditTargets(tool string, input *pyjson.Object, repo, file string) bool {
	if input == nil {
		return false
	}
	target, err := filepath.EvalSymlinks(filepath.Join(repo, file))
	if err != nil {
		return false
	}
	matches := func(path string) bool {
		if path == "" {
			return false
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(repo, path)
		}
		actual, err := filepath.EvalSymlinks(path)
		return err == nil && actual == target
	}
	if tool == "Edit" {
		path, _ := input.Value("file_path").(string)
		return matches(path)
	}
	if tool != "apply_patch" {
		return false
	}
	patch, command := input.Value("command").(string)
	legacy, rawInput := input.Value("input").(string)
	if command == rawInput {
		return false
	}
	if !command {
		patch = legacy
	}
	if !strings.HasPrefix(patch, "*** Begin Patch\n") {
		return false
	}
	updates, matched := 0, false
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "*** Add File: ") || strings.HasPrefix(line, "*** Delete File: ") {
			return false
		}
		if strings.HasPrefix(line, "*** Update File: ") {
			updates++
			matched = matches(strings.TrimPrefix(line, "*** Update File: "))
		}
	}
	return updates == 1 && matched
}

func TestSnapEditTargetsUsesPathsAndPatchHeaders(t *testing.T) {
	repo := t.TempDir()
	foreign := filepath.Join(t.TempDir(), "codec.py")
	for _, path := range []string{filepath.Join(repo, "codec.py"), filepath.Join(repo, "greet.py"), foreign} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "fixture")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: " + filepath.Join(repo, "codec.py") + "\n@@\n-ENCODING = None\n+ENCODING = 'utf-8'\n*** End Patch"
	for _, test := range []struct {
		name, tool string
		input      *pyjson.Object
		want       bool
	}{
		{"Claude target", "Edit", pyjson.NewObject("file_path", filepath.Join(repo, "codec.py")), true},
		{"Claude relative target", "Edit", pyjson.NewObject("file_path", "codec.py"), true},
		{"Claude symlink target", "Edit", pyjson.NewObject("file_path", filepath.Join(alias, "codec.py")), true},
		{"Claude content", "Edit", pyjson.NewObject("file_path", filepath.Join(repo, "greet.py"), "old_string", patch), false},
		{"Claude foreign basename", "Edit", pyjson.NewObject("file_path", foreign), false},
		{"Claude missing file", "Edit", pyjson.NewObject("file_path", "missing/codec.py"), false},
		{"Codex target", "apply_patch", pyjson.NewObject("input", patch), true},
		{"Codex command target", "apply_patch", pyjson.NewObject("command", patch), true},
		{"Codex relative target", "apply_patch", pyjson.NewObject("command", strings.Replace(patch, filepath.Join(repo, "codec.py"), "codec.py", 1)), true},
		{"Codex foreign basename", "apply_patch", pyjson.NewObject("command", strings.Replace(patch, filepath.Join(repo, "codec.py"), foreign, 1)), false},
		{"Codex combined edits", "apply_patch", pyjson.NewObject("command", strings.Replace(patch, "*** End Patch", "*** Update File: greet.py\n@@\n-Hello\n+Hi\n*** End Patch", 1)), false},
		{"Codex added file", "apply_patch", pyjson.NewObject("command", strings.Replace(patch, "*** End Patch", "*** Add File: extra.py\n+pass\n*** End Patch", 1)), false},
		{"Codex deleted file", "apply_patch", pyjson.NewObject("command", strings.Replace(patch, "*** End Patch", "*** Delete File: greet.py\n*** End Patch", 1)), false},
		{"Codex content", "apply_patch", pyjson.NewObject("input", "*** Begin Patch\n*** Update File: greet.py\n@@\n+codec.py\n*** End Patch"), false},
		{"Codex command content", "apply_patch", pyjson.NewObject("command", "*** Begin Patch\n*** Update File: greet.py\n@@\n+codec.py\n*** End Patch"), false},
		{"ambiguous patch fields", "apply_patch", pyjson.NewObject("command", patch, "input", patch), false},
		{"ordinary tool", "Bash", pyjson.NewObject("input", patch), false},
		{"missing input", "apply_patch", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := snapEditTargets(test.tool, test.input, repo, "codec.py"); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}
