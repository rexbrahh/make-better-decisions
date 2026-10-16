package evidence

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

func subQEvidenceProfile(t *testing.T) policy.EvidenceProfile {
	t.Helper()
	provider, err := policy.Provider("subq")
	if err != nil {
		t.Fatal(err)
	}
	return provider.Evidence
}

func TestSubQProfileRetainsEvidencePastLegacyBudget(t *testing.T) {
	profile := subQEvidenceProfile(t)
	output := strings.Repeat("e", 60_000)
	tx := transcript.Transcript{
		UserMessages: []string{"Preserve both observations."},
		ToolCalls:    []transcript.ToolCall{call("first", "Read", "one.txt", output), call("second", "Read", "two.txt", output)},
	}
	bundle := build(t, tx, nil, Options{Profile: profile})
	if got := dumpedChars(t, bundle, true); got <= policy.EvidenceChars {
		t.Fatalf("SubQ bundle retained only %d serialized characters", got)
	}
	if metadataInt(t, bundle, "evidence_budget_chars") != int64(profile.BudgetChars) ||
		metadataInt(t, bundle, "text_clip_chars") != int64(profile.TextChars) {
		t.Fatalf("profile metadata is %s", dumps(t, bundle.Value("metadata")))
	}
	if got := field(objects(bundle, "tool_calls_newest_first"), "id"); len(got) != 2 {
		t.Fatalf("retained calls are %q", got)
	}

	legacy := build(t, tx, nil, Options{})
	if got := dumpedChars(t, legacy, true); got > policy.EvidenceChars {
		t.Fatalf("SubQ profile changed the following default build: %d characters", got)
	}
}

func TestSubQProfileKeepsLongSourceTextHashAndProvenance(t *testing.T) {
	dir := stateDir(t)
	profile := subQEvidenceProfile(t)
	original := strings.Repeat("x", 200_000)
	writeFile(t, filepath.Join(dir, "large.py"), original)
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read large.py"}}, nil,
		Options{Query: "Read large.py", Cwd: &dir, Profile: profile})
	item := firstSource(t, bundle)
	text := item.Value("text").(string)
	if got := pystr.Len(text); got <= policy.ClipChars || got > profile.TextChars+100 {
		t.Fatalf("source excerpt has %d characters", got)
	}
	if item.Value("sha256") != sha256Text(original) || item.Value("truncated") != true ||
		item.Value("source") != "hook_file_read" || item.Value("role") != "working_tree_source" ||
		item.Value("trust") != "verified_file_observation" {
		t.Fatalf("source provenance is %s", dumps(t, item))
	}
}

func TestSubQProfileRedactsCompleteSourceBeforeClipping(t *testing.T) {
	dir := stateDir(t)
	profile := subQEvidenceProfile(t)
	secret := "local-credential-" + strings.Repeat("q", 32)
	cut := profile.TextChars/2 - 8
	original := strings.Repeat("x", cut) + secret + strings.Repeat("y", 180_000-cut-len(secret))
	writeFile(t, filepath.Join(dir, "boundary.py"), original)
	bundle := build(t, transcript.Transcript{UserMessages: []string{"Read boundary.py"}}, []string{secret},
		Options{Query: "Read boundary.py", Cwd: &dir, Profile: profile})
	got := firstSource(t, bundle).Value("text").(string)
	if strings.Contains(got, secret) || strings.Contains(got, secret[:8]) {
		t.Fatal("a clipped fragment of a secret survived source redaction")
	}
}

func TestTrimToBudgetWithProfileCountsUTF8AndJSONEscapes(t *testing.T) {
	profile := subQEvidenceProfile(t)
	for _, test := range []struct {
		name      string
		text      string
		shouldFit bool
	}{
		{name: "unicode within serialized budget", text: strings.Repeat("λ", 100_000), shouldFit: true},
		{name: "unicode escape expansion", text: strings.Repeat("λ", 150_000)},
		{name: "JSON special characters", text: strings.Repeat("\"\\\n", 140_000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := json.NewObject("user_messages", []any{test.text}, "metadata", json.NewObject())
			before := dumpedChars(t, bundle, true)
			if (before <= profile.BudgetChars) != test.shouldFit {
				t.Fatalf("fixture serialized to %d characters", before)
			}
			if err := TrimToBudgetWithProfile(bundle, nil, "", 0, profile); err != nil {
				t.Fatal(err)
			}
			if got := dumpedChars(t, bundle, true); got > profile.BudgetChars {
				t.Fatalf("trimmed JSON has %d characters", got)
			}
			got := bundle.Value("user_messages").([]any)[0].(string)
			if test.shouldFit && got != test.text {
				t.Fatal("text within the serialized budget was clipped")
			}
			if !test.shouldFit && (got == test.text || pystr.Len(got) < 10_000) {
				t.Fatalf("JSON escape expansion was not accounted for: retained %d code points", pystr.Len(got))
			}
		})
	}
}

func TestZeroProfileAndLegacyWrappersKeepDefaultBehavior(t *testing.T) {
	tx := transcript.Transcript{UserMessages: []string{"Preserve the default excerpt."},
		ToolCalls: []transcript.ToolCall{call("read", "Read", "input", strings.Repeat("λ", policy.ClipChars+100))}}
	defaultBuild := build(t, tx, nil, Options{})
	zeroBuild := build(t, tx, nil, Options{Profile: policy.EvidenceProfile{}})
	if dumps(t, defaultBuild) != dumps(t, zeroBuild) {
		t.Fatal("zero profile changed default Build output")
	}
	if metadataInt(t, zeroBuild, "evidence_budget_chars") != policy.EvidenceChars ||
		metadataInt(t, zeroBuild, "text_clip_chars") != policy.ClipChars {
		t.Fatalf("zero profile metadata is %s", dumps(t, zeroBuild.Value("metadata")))
	}

	makeBundle := func() *json.Object {
		return json.NewObject("user_messages", []any{strings.Repeat("x", policy.EvidenceChars+1_000)},
			"metadata", json.NewObject())
	}
	legacyTrim, profileTrim := makeBundle(), makeBundle()
	if err := TrimToBudget(legacyTrim, nil, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := TrimToBudgetWithProfile(profileTrim, nil, "", 0, policy.EvidenceProfile{}); err != nil {
		t.Fatal(err)
	}
	if dumps(t, legacyTrim) != dumps(t, profileTrim) {
		t.Fatal("zero-profile trimming differs from the legacy wrapper")
	}

	dir := stateDir(t)
	writeFile(t, filepath.Join(dir, "contract.md"), "Preserve the contract hash.\n")
	legacyObservations, err := ContractObservations([]string{"contract.md"}, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	profileObservations, err := ContractObservationsWithProfile([]string{"contract.md"}, dir, nil,
		policy.EvidenceProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if dumps(t, legacyObservations) != dumps(t, profileObservations) {
		t.Fatal("zero-profile contract observations differ from the legacy wrapper")
	}
}

func TestInvalidEvidenceProfilesFailClosed(t *testing.T) {
	invalid := policy.EvidenceProfile{BudgetChars: 2_000_000, TextChars: 1_000_000}
	if _, err := Build(transcript.Transcript{}, nil, Options{Profile: invalid}); err == nil {
		t.Fatal("Build accepted an unknown evidence profile")
	}
	bundle := json.NewObject("user_messages", []any{}, "metadata", json.NewObject())
	if err := TrimToBudgetWithProfile(bundle, nil, "", 0, invalid); err == nil {
		t.Fatal("trimming accepted an unknown evidence profile")
	}
	dir := stateDir(t)
	if _, err := ContractObservationsWithProfile(nil, dir, nil, invalid); err == nil {
		t.Fatal("contract observations accepted an unknown evidence profile")
	}
}
