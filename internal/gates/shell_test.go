package gates

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

func TestQuotedHeredocShellTruthControls(t *testing.T) {
	cases := []struct {
		name, command, kind string
	}{
		{"real_data_tail", "reader <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"double_delimiter_tail", "reader <<\"DATA\"\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"strip_tabs_tail", "reader <<-'DATA'\n\tts-decide '{}'\n\tDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"quoted_hash_data_only", "reader '#' <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"quoted_hash_tail", "reader '#' <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"escaped_hash_data_only", "reader \\# <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"escaped_hash_tail", "reader \\# <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"midword_hash_data_only", "reader file#literal <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"midword_hash_tail", "reader file#literal <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"empty_quote_hash_data_only", "reader ''#literal <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"empty_quote_hash_tail", "reader ''#literal <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"unicode_hash_data_only", "reader \"é#literal\" <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"unicode_hash_tail", "reader \"é#literal\" <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"literal_substitution_data_only", "reader '$(literal)' <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"literal_substitution_tail", "reader '$(literal)' <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"active_substitution_data_only", "reader \"$(printf x)\" <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"active_substitution_tail", "reader \"$(printf x)\" <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"backquote_comment_data_only", "reader \"`printf x # ignored`\" <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"backquote_comment_tail", "reader \"`printf x # ignored`\" <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"ansi_escaped_quote_data_only", "reader $'a\\'b' <<'DATA'\nts-decide '{}'\nDATA\n", "other"},
		{"ansi_escaped_quote_tail", "reader $'a\\'b' <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "other"},
		{"comment_marker_data_only", "reader # <<'DATA'\nts-decide '{}'\nDATA\n", "misuse"},
		{"comment_marker_tail", "reader # <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "misuse"},
		{"ansi_comment_marker_data_only", "reader $'a\\'b' # <<'DATA'\nts-decide '{}'\nDATA\n", "misuse"},
		{"ansi_comment_marker_tail", "reader $'a\\'b' # <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "misuse"},
		{"nested_comment_data_only", "reader \"$(printf x #)\" <<'DATA'\nts-decide '{}'\n)\"\nDATA\n", "misuse"},
		{"nested_comment_tail", "reader \"$(printf x #)\" <<'DATA'\nts-decide '{}'\n)\"\nDATA\nprintf 'TAIL_OK\\n'\n", "misuse"},
		{"data_then_consultation", "reader <<'DATA'\nts-decide '{}'\nDATA\nts-decide '{}'\n", "misuse"},
		{"env_split_ansi_words_data_only", "/usr/bin/env -S $'sh -' <<'DATA'\nts-decide '{}'\nDATA\n", "misuse"},
		{"env_split_ansi_words_tail", "/usr/bin/env -S $'sh -' <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "misuse"},
		{"env_split_ansi_escaped_space_data_only", "/usr/bin/env -S $'sh\\x20-' <<'DATA'\nts-decide '{}'\nDATA\n", "misuse"},
		{"env_split_ansi_escaped_space_tail", "/usr/bin/env -S $'sh\\x20-' <<'DATA'\nts-decide '{}'\nDATA\nprintf 'TAIL_OK\\n'\n", "misuse"},
		{"shell_stdin_nested_call", "/usr/bin/env -S $'sh -' <<'DATA'\nprintf '%s\\n' \"$(ts-decide '{}')\"\nDATA\n", "misuse"},
		{"shell_stdin_nested_call_tail", "/usr/bin/env -S $'sh -' <<'DATA'\nprintf '%s\\n' \"$(ts-decide '{}')\"\nDATA\nprintf 'TAIL_OK\\n'\n", "misuse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertClassify(t, c.command, c.kind, "")
		})
	}
}

func TestStaticShellHeadsDoNotExpandHostState(t *testing.T) {
	t.Setenv("CLASSIFIER_HEAD", "ts-decide")
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	for _, c := range []struct{ command, head string }{
		{"env -S $'sh\\x20-'", "sh"},
		{"command -v ts-decide", ""},
		{"env -u NAME /usr/bin/python3 -", "python3"},
		{"reader \"$(printf ignored)\"", "reader"},
		{"'$CLASSIFIER_HEAD'", "$CLASSIFIER_HEAD"},
		{"\"${CLASSIFIER_HEAD:-ts-decide}\"", ""},
		{"\"${CLASSIFIER_HEAD:-$(touch " + marker + ")}\"", ""},
		{"$(touch " + marker + ")", ""},
		{"`touch " + marker + "`", ""},
		{"<(touch " + marker + ")", ""},
		{"~root/ts-decide", ""},
		{"'~root/reader'", "reader"},
		{"$\"ts-decide\"", ""},
		{"ts-{decide,other}", ""},
		{"ts-*", ""},
		{"env -S \"$CLASSIFIER_HEAD\"", ""},
	} {
		file, err := syntax.NewParser().Parse(strings.NewReader(c.command), "")
		if err != nil {
			t.Fatal(err)
		}
		call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
		if !ok {
			t.Fatalf("not a call: %q", c.command)
		}
		if got := staticShellHead(call); got != c.head {
			t.Errorf("head of %q = %q, want %q", c.command, got, c.head)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("static resolution executed a substitution: %v", err)
	}
}

func TestShellViewsKeepOriginalCoordinatesAndRequests(t *testing.T) {
	for _, c := range []struct{ command, kind string }{
		{"# é\nreader $'a\\'b' <<'DATA'\nts-decide '{}'\nDATA\nprintf 'tail'\n", "other"},
		{"# é\nreader $'a\\'b' <<'DATA'\nts-decide '{}'\nDATA\nprintf '%s' \"$(ts-decide '{}')\"\n", "misuse"},
		{"reader <<'DATA'\nts-decide '{}'\nDATA\nprintf '\n", "other"},
		{"reader <<'DATA'\nts-decide '{}'\nDATA\nprintf \"$(ts-decide '{}')\"\nprintf '\n", "misuse"},
		{"\"${CLASSIFIER_HEAD:-sh}\" <<'DATA'\nts-decide '{}'\nDATA\n", "misuse"},
		{"reader # <<'DATA'\nts-decide '{}'\nDATA\nprintf '\n", "misuse"},
		{"printf '%s' $'a\\'b'; ts-decide '{}'", "misuse"},
	} {
		assertClassify(t, c.command, c.kind, "")
	}
	const raw = "{\"decision\":\"é\", \"options\":[\"a\",\"b\"]}"
	assertClassify(t, "ts-decide '"+raw+"'", "decide", raw)
	assertClassify(t, "ts-decide $'{}'", "decide", "${}") // Keep the legacy payload; do not decode it from the detection view.
	assertClassify(t, "ts-decide <<'EOF'\r\n"+raw+"\r\nEOF\r\n", "decide", raw)
	command := "# é\nreader $'a\\'b' <<'DATA'\nts-decide '{}'\nDATA\nprintf 'tail'\n"
	source, detection, called := shellCommandViews(command)
	if source != strings.Replace(command, "ts-decide '{}'\n", "", 1) || called || !strings.HasSuffix(detection, "DATA\nprintf 'tail'\n") {
		t.Fatalf("changed source coordinates or suffix: %q, %q, %v", source, detection, called)
	}
}

func TestShellParseBudgetAndPositionBounds(t *testing.T) {
	limit := uint64(syntax.NewPos(^uint(0), 1, 1).Offset())
	if !shellInputFits(limit) || shellInputFits(limit+1) {
		t.Fatal("saturated parser offsets accepted")
	}
	const ordinary = "printf '%s' ordinary\n"
	a := shellAnalysis{remaining: 8 * int64(len(ordinary)), ansi: make(map[shellSpan]string)}
	a.scan(ordinary, 0)
	if a.exhausted || a.remaining != 6*int64(len(ordinary)) {
		t.Fatalf("ordinary input was not parsed full-first once per dialect: %+v", a)
	}
	command := "reader <<'DATA'\nts-decide '{}'\nDATA\nprintf tail\n"
	a = shellAnalysis{remaining: int64(len(command) - 1), ansi: make(map[shellSpan]string)}
	a.scan(command, 0)
	if !a.exhausted || a.remaining < 0 || len(a.data) != 0 {
		t.Fatalf("exhausted budget excluded data: %+v", a)
	}
	for i := range 12 {
		command = fmt.Sprintf("sh <<'BODY%d'\n%sBODY%d\n", i, command, i)
	}
	source, _, _ := shellCommandViews(command)
	if source != command {
		t.Fatal("nested input exhaustion excluded original source")
	}
	assertClassify(t, command, "misuse", "")
}

func TestShellLongAndDeepInputsRemainBounded(t *testing.T) {
	long := "reader <<'DATA'\n" + strings.Repeat("ts-decide '{}'\n", 20_000) + "DATA\nprintf tail\n"
	assertClassify(t, long, "other", "")
	deep := strings.Repeat("(", 10_001) + "printf ok" + strings.Repeat(")", 10_001)
	assertClassify(t, deep, "other", "")
	assertClassify(t, "ts-decide '{}'; "+deep, "misuse", "")
}

func TestShellClassificationIsConcurrent(t *testing.T) {
	commands := []struct{ command, kind string }{
		{"reader <<'DATA'\nts-decide '{}'\nDATA\nprintf tail\n", "other"},
		{"reader $'a\\'b' # <<'DATA'\nts-decide '{}'\nDATA\n", "misuse"},
		{"env -S $'sh\\x20-' <<'DATA'\nprintf '%s' \"$(ts-decide '{}')\"\nDATA\n", "misuse"},
	}
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			for _, c := range commands {
				assertClassify(t, c.command, c.kind, "")
			}
		})
	}
	workers.Wait()
}

func TestShellSyntaxFindsNegationAndContinuedNames(t *testing.T) {
	for _, head := range []string{"ts-decide", "! ts-decide", "ts-decid\\\ne", "! ts-decid\\\ne"} {
		for _, prefix := range []string{"", "# comment\n", "printf ok\n"} {
			for _, suffix := range []string{"; printf tail", " | cat", "\nprintf tail"} {
				assertClassify(t, prefix+head+" '{}'"+suffix, "misuse", "")
			}
		}
	}
	// Keep the existing conservative treatment of shell-named consumers.
	assertClassify(t, "sh -c 'printf ok' <<'DATA'\nts-decide '{}'\nDATA\n", "misuse", "")
}
