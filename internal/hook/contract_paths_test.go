package hook

import (
	stdcontext "context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

func contractPathFixture(t *testing.T, provider string) rcContinuity {
	t.Helper()
	c := rcContinuitySetUp(t)
	t.Setenv("SUBQ_API_KEY", "snap-test-credential-0123456789")
	patch(t, &newSnapClient, func(stdcontext.Context) reviewClient {
		return reviewClientFunc(func(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error) {
			return jev.Ask(state, questions, key, fake(map[string]any{"pick": "chunks", "sufficient": .95,
				"owner": "agent", "invariant_0_chunks": .95, "invariant_0_whole": .95,
				"needs_decision": .99, "covered": .99, "writes_files": .99}).Post, metrics)
		})
	})
	if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", "codex", "review_provider", provider))); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(c.dir, "docs", "contract.md"), readFile(t, c.contract))
	c.contract = filepath.Join(c.dir, "docs", "contract.md")
	for alias, target := range map[string]string{"link.md": "docs/contract.md", "linked-docs": "docs", "private.md": ".env"} {
		if err := os.Symlink(target, filepath.Join(c.dir, alias)); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(c.dir, ".env"), "SYNTHETIC_TOKEN=private-fixture\n")
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeFile(t, outside, "Outside fixture.\n")
	if err := os.Symlink(outside, filepath.Join(c.dir, "outside.md")); err != nil {
		t.Fatal(err)
	}
	return c
}

func contractPathRequest(paths []string) *json.Object {
	return json.NewObject("decision", "Which lease representation should parsing use?", "options", []any{
		json.NewObject("id", "shared", "description", "Reference-counted lease"),
		json.NewObject("id", "owned", "description", "Single-owner lease")},
		"scope", json.NewObject("paths", []any{"parser.py"}, "invariants", []any{"Retain active leases until work ends."}),
		"contracts", paths)
}

func contractPathContext(t *testing.T, c rcContinuity, paths []string) *gates.Context {
	t.Helper()
	value := json.NewObject("command", "ts-decide <<'EOF'\n"+dumps(t, contractPathRequest(paths))+"\nEOF")
	ctx, err := context(c.payload("Bash", value, "s1"), "codex", paths)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestContractPathSpellingsReachReview(t *testing.T) {
	for _, provider := range []string{"jev", "subq"} {
		for _, tc := range []struct {
			path, key string
			available bool
		}{
			{"docs/contract.md", "docs/contract.md", true},
			{"./docs/contract.md", "docs/contract.md", true},
			{"docs/./contract.md", "docs/contract.md", true},
			{"docs//contract.md", "docs/contract.md", true},
			{"link.md", "link.md", true},
			{"./link.md", "link.md", true},
			{"linked-docs/contract.md", "linked-docs/contract.md", true},
			{"missing.md", "missing.md", false},
			{"outside.md", "outside.md", false},
			{"private.md", "private.md", false},
		} {
			t.Run(provider+"/"+tc.path, func(t *testing.T) {
				c := contractPathFixture(t, provider)
				paths := []string{tc.path}
				ctx := contractPathContext(t, c, paths)
				api := fake(map[string]any{"pick": "shared", "owner": "agent", "sufficient": .95,
					"invariant_0_shared": .95, "invariant_0_owned": .95})
				out, err := gates.Decide(dumps(t, contractPathRequest(paths)), *ctx, gatesAsker(api))
				if err != nil {
					t.Fatal(err)
				}
				if !tc.available {
					assertEqual(t, out.Action, "gather_evidence")
					assertEqual(t, len(api.Requests()), 0)
					if _, ok := out.Record.Get("receipt_id"); ok {
						t.Fatal("an unavailable contract produced a receipt")
					}
					return
				}
				assertEqual(t, out.Action, "advisory")
				assertEqual(t, len(api.Requests()), 1)
				want := json.NewObject("docs/contract.md", rcSha256(t, c.contract))
				want.Set(tc.key, rcSha256(t, c.contract))
				assertEqual(t, out.Record.Value("contracts"), want)
			})
		}
	}
}

func TestLexicalContractSpellingsReuseScopedGather(t *testing.T) {
	for _, provider := range []string{"jev", "subq"} {
		for _, group := range []struct {
			name  string
			paths [][]string
		}{
			{"file", [][]string{{"docs/contract.md"}, {"./docs/contract.md"}, {"docs//./contract.md"}, {"./docs/contract.md", "docs/contract.md"}}},
			{"symlink", [][]string{{"link.md"}, {"./link.md"}, {"link.md", "./link.md"}}},
			{"directory", [][]string{{"linked-docs/contract.md"}, {"./linked-docs//contract.md"}}},
			{"multiple aliases", [][]string{{"link.md", "linked-docs/contract.md"}, {"linked-docs/contract.md", "link.md"}, {"./link.md", "./linked-docs//contract.md", "link.md"}}},
		} {
			t.Run(provider+"/"+group.name, func(t *testing.T) {
				c := contractPathFixture(t, provider)
				var first gates.Outcome
				for i, paths := range group.paths {
					ctx := contractPathContext(t, c, paths)
					if i > 0 {
						ctx.Entries = append(ctx.Entries, with(first.Record, "host", "codex", "agent_id", nil,
							"action", first.Action, "verdict", first.Message))
					}
					api := fake(map[string]any{"pick": "shared", "owner": "agent", "sufficient": .95,
						"invariant_0_shared": .40, "invariant_0_owned": .40})
					out, err := gates.Decide(dumps(t, contractPathRequest(paths)), *ctx, gatesAsker(api))
					if err != nil {
						t.Fatal(err)
					}
					assertEqual(t, out.Action, "gather_evidence")
					if i == 0 {
						assertEqual(t, len(api.Requests()), 1)
						first = out
					} else {
						assertEqual(t, len(api.Requests()), 0)
						if !strings.Contains(out.Message, "reused:") {
							t.Fatalf("GATHER was not reused: %s", out.Message)
						}
					}
				}
			})
		}
	}
}

func TestContractAliasChangesInvalidateCompletedAndInheritedCoverage(t *testing.T) {
	for _, provider := range []string{"jev", "subq"} {
		for _, change := range []string{"target content", "alias target", "alias private", "alias outside", "alias missing", "target missing"} {
			t.Run(provider+"/"+change, func(t *testing.T) {
				c := contractPathFixture(t, provider)
				prepared := rcPrepare(t, c.dir, rcPrepareOptions{opaque: true, contracts: []string{"./link.md"}})
				parent := json.NewObject("session_id", "root-session", "tool_use_id", "parent-edit", "tool_name", "Edit",
					"tool_input", json.NewObject("file_path", "parser.py"), "cwd", c.dir,
					"transcript_path", filepath.Join(c.dir, "parent.jsonl"))
				for _, payload := range []*json.Object{parent, prepared.payload()} {
					assertEqual(t, len(rcSummaries(t, rcContext(t, payload, "codex"))), 1)
				}

				link := filepath.Join(c.dir, "link.md")
				switch change {
				case "target content":
					writeFile(t, c.contract, "Release the lease after Flush.\n")
				case "target missing":
					if err := os.Remove(c.contract); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
					if change != "alias missing" {
						target := map[string]string{"alias target": "other.md", "alias private": ".env", "alias outside": "outside.md"}[change]
						writeFile(t, filepath.Join(c.dir, "other.md"), "Keep the lease until Close.\n")
						if err := os.Symlink(target, link); err != nil {
							t.Fatal(err)
						}
					}
				}
				for _, payload := range []*json.Object{parent, prepared.payload()} {
					assertEqual(t, len(rcSummaries(t, rcContext(t, payload, "codex"))), 0)
					assertEqual(t, decision(t, handle(t, "pre_tool_use", payload, "codex", rcCovered(.99).Post)), "deny")
				}
			})
		}
	}
}

func TestEightContractAliasesKeepCompletedAndInheritedCoverage(t *testing.T) {
	for _, provider := range []string{"jev", "subq"} {
		t.Run(provider, func(t *testing.T) {
			c := contractPathFixture(t, provider)
			var contracts []string
			for i := range 8 {
				target := filepath.Join(c.dir, "docs", fmt.Sprintf("contract-%d.md", i))
				writeFile(t, target, "Retain active leases until work ends.\n")
				alias := fmt.Sprintf("alias-%d.md", i)
				if err := os.Symlink(target, filepath.Join(c.dir, alias)); err != nil {
					t.Fatal(err)
				}
				contracts = append(contracts, "./"+alias)
			}
			prepared := rcPrepare(t, c.dir, rcPrepareOptions{opaque: true, contracts: contracts})
			parent := json.NewObject("session_id", "root-session", "tool_use_id", "parent-edit", "tool_name", "Edit",
				"tool_input", json.NewObject("file_path", "parser.py"), "cwd", c.dir,
				"transcript_path", filepath.Join(c.dir, "parent.jsonl"))
			for _, payload := range []*json.Object{parent, prepared.payload()} {
				assertEqual(t, len(rcSummaries(t, rcContext(t, payload, "codex"))), 1)
			}
			writeFile(t, filepath.Join(c.dir, "docs", "contract-7.md"), "Release the lease after Flush.\n")
			for _, payload := range []*json.Object{parent, prepared.payload()} {
				assertEqual(t, len(rcSummaries(t, rcContext(t, payload, "codex"))), 0)
			}
		})
	}
}
