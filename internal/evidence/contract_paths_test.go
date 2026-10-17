package evidence

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

func TestSymlinkContractsSurviveBudgetAndShortening(t *testing.T) {
	for _, name := range []string{"jev", "subq"} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			provider, err := policy.Provider(name)
			mustDo(t, err)
			mustDo(t, os.Mkdir(filepath.Join(dir, "links"), 0o755))
			var contracts []string
			hashes := map[string]string{}
			for i := range 8 {
				path := fmt.Sprintf("docs/contract-%d.md", i)
				text := strings.Repeat("Preserve the public contract.\n", provider.Evidence.TextChars/20)
				writeFile(t, filepath.Join(dir, path), text)
				hashes[path] = sha256Text(text)
				alias := fmt.Sprintf("links/contract-%d.md", i)
				mustDo(t, os.Symlink("../"+path, filepath.Join(dir, alias)))
				contracts = append(contracts, "./"+alias)
			}
			calls := []transcript.ToolCall{call("read", "Bash", "inspect helpers", strings.Repeat("observation\n", 2_000))}
			bundle := build(t, transcript.Transcript{UserMessages: []string{"Preserve all contracts. " + strings.Repeat("u", 20_000)},
				ToolCalls: calls, TurnCalls: calls}, nil, Options{Cwd: &dir, Contracts: contracts, Profile: provider.Evidence})
			if got := hashesByPath(bundle); !maps.Equal(got, hashes) {
				t.Fatalf("required source hashes: %v, want %v", got, hashes)
			}
			mustDo(t, TrimToBudgetWithProfile(bundle, contracts, "", 0, provider.Evidence))
			if got := hashesByPath(bundle); !maps.Equal(got, hashes) {
				t.Fatalf("required source hashes after trimming: %v", got)
			}
			if n := dumpedChars(t, bundle, true); n > provider.Evidence.BudgetChars {
				t.Fatalf("evidence has %d characters, limit %d", n, provider.Evidence.BudgetChars)
			}

			texts := map[string]any{}
			for _, source := range objects(bundle, "source_files") {
				texts[source.Value("path").(string)] = source.Value("text")
			}
			bundle.Set("tool_calls_newest_first", []any{json.NewObject("input", strings.Repeat("i", 4_000),
				"output", strings.Repeat("o", 4_000))})
			shortened, err := ShortenObservations(bundle, "", contracts)
			mustDo(t, err)
			if shortened == nil {
				t.Fatal("optional observations were not shortened")
			}
			for _, source := range objects(shortened, "source_files") {
				if source.Value("text") != texts[source.Value("path").(string)] || source.Value("trust") != "verified_file_observation" {
					t.Fatalf("shortening changed a required source: %s", source.Value("path"))
				}
			}
		})
	}
}

func TestContractAliasesShareFreshnessReadBudget(t *testing.T) {
	for _, name := range []string{"jev", "subq"} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			provider, err := policy.Provider(name)
			mustDo(t, err)
			var paths []string
			hashes := map[string]string{}
			for i := range 8 {
				target, alias := fmt.Sprintf("target-%d.md", i), fmt.Sprintf("alias-%d.md", i)
				text := fmt.Sprintf("Preserve contract %d.\n", i)
				writeFile(t, filepath.Join(dir, target), text)
				mustDo(t, os.Symlink(target, filepath.Join(dir, alias)))
				paths = append(paths, alias, target)
				hashes[alias], hashes[target] = sha256Text(text), sha256Text(text)
			}
			reads := 0
			original := openSourceFile
			patch(t, &openSourceFile, func(path, root string) (sourceFile, error) {
				reads++
				return original(path, root)
			})
			observed, err := ContractObservationsWithProfile(paths, dir, nil, provider.Evidence)
			mustDo(t, err)
			if observed.Len() != 16 || reads != 8 {
				t.Fatalf("observed %d names with %d reads; want 16 names with 8 reads", observed.Len(), reads)
			}
			for path, hash := range hashes {
				item := observed.Value(path).(*json.Object)
				if item.Value("sha256") != hash || item.Value("trust") != "verified_file_observation" {
					t.Fatalf("incorrect freshness for %s", path)
				}
			}
		})
	}
}

func TestRetargetedContractAliasIsNotVerified(t *testing.T) {
	for _, providerName := range []string{"jev", "subq"} {
		for _, phase := range []string{"capture", "freshness"} {
			t.Run(providerName+"/"+phase, func(t *testing.T) {
				dir := stateDir(t)
				provider, err := policy.Provider(providerName)
				mustDo(t, err)
				writeFile(t, filepath.Join(dir, "first.md"), "Retain the lease.\n")
				writeFile(t, filepath.Join(dir, "second.md"), "Release the lease.\n")
				link := filepath.Join(dir, "link.md")
				mustDo(t, os.Symlink("first.md", link))
				swapped := false
				original := resolveSource
				patch(t, &resolveSource, func(root, raw string) (string, bool, error) {
					path, ok, err := original(root, raw)
					if raw == "link.md" && ok && !swapped {
						swapped = true
						mustDo(t, os.Remove(link))
						mustDo(t, os.Symlink("second.md", link))
					}
					return path, ok, err
				})
				if phase == "capture" {
					bundle := build(t, transcript.Transcript{UserMessages: []string{"Preserve the contract."}}, nil,
						Options{Cwd: &dir, Contracts: []string{"link.md"}, Profile: provider.Evidence})
					for _, item := range objects(bundle, "source_files") {
						for _, path := range SourcePaths(item) {
							if path == "link.md" {
								t.Fatal("the retargeted alias retained a verified binding to the old contents")
							}
						}
					}
				} else {
					observed, err := ContractObservationsWithProfile([]string{"link.md"}, dir, nil, provider.Evidence)
					mustDo(t, err)
					item := observed.Value("link.md").(*json.Object)
					if item.Value("sha256") != nil || item.Value("trust") != "unavailable" {
						t.Fatal("freshness verified old contents under the retargeted alias")
					}
				}
				if !swapped {
					t.Fatal("the retarget schedule did not run")
				}
			})
		}
	}
}
