package main

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestProviderChooserStartsWithTheModelChoice(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider"}, strings.NewReader("\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	text := stdout.String()
	if !strings.HasPrefix(text, "Choose your review model:\n1) Jev") || !strings.Contains(text, "2) SubQ Snap") {
		t.Fatalf("missing first model choice: %q", text)
	}
	if strings.Contains(text, "Choose the host") || stderr.Len() != 0 {
		t.Fatalf("cancel continued: stdout=%q stderr=%q", text, stderr.String())
	}
}

func TestProviderChooserCancelsBeforePlanning(t *testing.T) {
	for _, input := range []string{"", " \t\n", "1\n", "2\n\n", "2\n \t\n"} {
		t.Run(input, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{"provider"}, strings.NewReader(input), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Cancelled. No changes applied.") || stderr.Len() != 0 {
				t.Fatalf("cancel: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestProviderChooserAppliesTheConfirmedModelAndHost(t *testing.T) {
	for _, test := range []struct{ name, input, host, provider string }{
		{"jev_codex", "1\n1\napply\n", "codex", "jev"},
		{"jev_claude", "1\n2\napply\n", "claude", "jev"},
		{"jev_both", "1\n3\napply\n", "both", "jev"},
		{"snap_codex", "2\n1\napply\n", "codex", "subq"},
		{"snap_claude", "2\n2\napply\n", "claude", "subq"},
		{"snap_both", "2\n3\napply\n", "both", "subq"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			installer := func(args []string, out, errout io.Writer) int {
				calls++
				want := []string{"--host", test.host, "--review-provider", test.provider}
				if calls == 2 {
					want = append(want, "--apply")
					preview := strings.Index(stdout.String(), "sanitized-plan-fixture\n")
					confirmation := strings.Index(stdout.String(), "Type apply")
					if preview < 0 || confirmation <= preview {
						t.Fatal("activation preceded the visible preview and confirmation")
					}
				}
				if !slices.Equal(args, want) || calls > 2 {
					t.Fatalf("installer call %d: %v, want %v", calls, args, want)
				}
				fmt.Fprintln(out, "sanitized-plan-fixture")
				return 0
			}
			if code := runProvider(nil, strings.NewReader(test.input), &stdout, &stderr, installer); code != 0 || calls != 2 {
				t.Fatalf("exit %d, installer calls %d, stderr %s", code, calls, stderr.String())
			}
		})
	}
}

func TestProviderChooserDoesNotApplyWithoutACompleteConfirmation(t *testing.T) {
	for name, answer := range map[string]string{"blank": "\n", "eof": "", "unfinished": "apply", "other": "yes\n", "uppercase": "APPLY\n"} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			installer := func(args []string, out, errout io.Writer) int {
				calls++
				if slices.Contains(args, "--apply") {
					t.Fatal("cancelled input reached installer activation")
				}
				return 0
			}
			code := runProvider(nil, strings.NewReader("2\n3\n"+answer), &stdout, &stderr, installer)
			if code != 0 || calls != 1 || !strings.Contains(stdout.String(), "Cancelled.") {
				t.Fatalf("exit %d, installer calls %d, output %q", code, calls, stdout.String())
			}
		})
	}
}

func TestProviderChooserPropagatesInstallerFailures(t *testing.T) {
	for _, failCall := range []int{1, 2} {
		t.Run(fmt.Sprint(failCall), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			installer := func(args []string, out, errout io.Writer) int {
				calls++
				if calls == failCall {
					fmt.Fprintln(errout, "fixture installer failure")
					return 19
				}
				return 0
			}
			code := runProvider(nil, strings.NewReader("2\n3\napply\n"), &stdout, &stderr, installer)
			if code != 19 || calls != failCall || !strings.Contains(stderr.String(), "fixture installer failure") {
				t.Fatalf("exit %d, installer calls %d, stderr %q", code, calls, stderr.String())
			}
			if failCall == 1 && strings.Contains(stdout.String(), "Type apply") {
				t.Fatal("failed preview reached confirmation")
			}
		})
	}
}

func TestProviderChooserRejectsInvalidChoicesWithoutEchoingThem(t *testing.T) {
	const sensitive = "synthetic-sensitive-input-0123456789"
	for _, input := range []string{sensitive + "\n", "1\n" + sensitive + "\n"} {
		var stdout, stderr bytes.Buffer
		installer := func([]string, io.Writer, io.Writer) int {
			t.Fatal("invalid choice reached installer")
			return 0
		}
		code := runProvider(nil, strings.NewReader(input), &stdout, &stderr, installer)
		if code != 2 || strings.Contains(stdout.String()+stderr.String(), sensitive) {
			t.Fatalf("invalid input: exit %d, output %q", code, stdout.String()+stderr.String())
		}
	}
}

func TestProviderChooserStopsOnInputFailure(t *testing.T) {
	for _, input := range []io.Reader{iotest.ErrReader(io.ErrUnexpectedEOF), strings.NewReader(strings.Repeat("x", 8192) + "\n")} {
		var stdout, stderr bytes.Buffer
		installer := func([]string, io.Writer, io.Writer) int {
			t.Fatal("input failure reached installer")
			return 0
		}
		if code := runProvider(nil, input, &stdout, &stderr, installer); code != 1 || stderr.Len() == 0 {
			t.Fatalf("input error: exit %d, stderr %q", code, stderr.String())
		}
	}
}

type providerWriterFunc func([]byte) (int, error)

func (w providerWriterFunc) Write(p []byte) (int, error) { return w(p) }

func TestProviderChooserDoesNotApplyWhenThePreviewCannotPrint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writer := providerWriterFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("sanitized-plan-fixture")) {
			return 0, io.EOF
		}
		return stdout.Write(p)
	})
	calls := 0
	installer := func(args []string, out, errout io.Writer) int {
		calls++
		if slices.Contains(args, "--apply") {
			t.Fatal("invisible preview reached activation")
		}
		fmt.Fprintln(out, "sanitized-plan-fixture")
		return 0
	}
	code := runProvider(nil, strings.NewReader("2\n3\napply\n"), writer, &stderr, installer)
	if code != 1 || calls != 1 || strings.Contains(stdout.String(), "Type apply") {
		t.Fatalf("preview write error: exit %d, installer calls %d, output %q", code, calls, stdout.String())
	}
}
