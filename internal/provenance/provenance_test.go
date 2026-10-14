package provenance

import (
	"reflect"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

func makeTranscript(users []string, calls []transcript.ToolCall, brief *string) transcript.Transcript {
	return transcript.Transcript{UserMessages: users, ToolCalls: calls, TurnCalls: calls, Brief: brief}
}

func verify(t *testing.T, facts []string, tr transcript.Transcript) (verified, unverified []string) {
	t.Helper()
	verified, unverified, err := Verify(facts, tr)
	if err != nil {
		t.Fatal(err)
	}
	return verified, unverified
}

func check(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) || len(got) > 0 && !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// py: tests/test_provenance.py::ProvenanceTest::test_verifies_user_quotes_but_not_arbitrary_tool_output
func TestVerifiesUserQuotesButNotArbitraryToolOutput(t *testing.T) {
	tr := makeTranscript([]string{"Keep memory   under 2 GB please"},
		[]transcript.ToolCall{{ID: "t1", Name: "Bash", Input: "free", Output: "Tiles are re-read in BURSTS during panning"}}, nil)
	verified, unverified := verify(t, []string{"keep memory under 2 gb", "tiles are re-read in bursts"}, tr)
	check(t, verified, []string{"keep memory under 2 gb"})
	check(t, unverified, []string{"tiles are re-read in bursts"})
}

// py: tests/test_provenance.py::ProvenanceTest::test_fabricated_quote_is_unverified
func TestFabricatedQuoteIsUnverified(t *testing.T) {
	tr := makeTranscript([]string{"reduce memory use"},
		[]transcript.ToolCall{{ID: "t1", Name: "Bash", Input: "pytest", Output: "3 failed, 37 passed"}}, nil)
	verified, unverified := verify(t, []string{"all 40 tests pass"}, tr)
	check(t, verified, nil)
	check(t, unverified, []string{"all 40 tests pass"})
}

// py: tests/test_provenance.py::ProvenanceTest::test_quote_found_only_in_an_earlier_ts_decide_call_is_unverified
func TestQuoteFoundOnlyInAnEarlierTsDecideCallIsUnverified(t *testing.T) {
	fact := "the user said memory is not a concern"
	tr := makeTranscript([]string{"reduce memory use"}, []transcript.ToolCall{{ID: "t1", Name: "Bash",
		Input: `{"command": "ts-decide '{\"facts\": [\"` + fact + `\"]}'"}`, Output: "echo: " + fact}}, nil)
	verified, unverified := verify(t, []string{fact}, tr)
	check(t, verified, nil)
	check(t, unverified, []string{fact})
}

// py: tests/test_provenance.py::ProvenanceTest::test_short_fact_is_never_verified
func TestShortFactIsNeverVerified(t *testing.T) {
	verified, unverified := verify(t, []string{"yes"}, makeTranscript([]string{"yes, go ahead"}, nil, nil))
	check(t, verified, nil)
	check(t, unverified, []string{"yes"})
}

// py: tests/test_provenance.py::ProvenanceTest::test_tool_input_is_not_a_source
func TestToolInputIsNotASource(t *testing.T) {
	tr := makeTranscript([]string{"go"},
		[]transcript.ToolCall{{ID: "t1", Name: "Write", Input: "the cache must use LFU eviction", Output: "File created"}}, nil)
	verified, _ := verify(t, []string{"the cache must use LFU eviction"}, tr)
	check(t, verified, nil)
}

// py: tests/test_provenance.py::ProvenanceTest::test_subagent_brief_is_not_a_user_source
func TestSubagentBriefIsNotAUserSource(t *testing.T) {
	brief := "The parent says the bug reproduces only on arm64"
	verified, _ := verify(t, []string{"reproduces only on arm64"}, makeTranscript(nil, nil, &brief))
	check(t, verified, nil)
}
