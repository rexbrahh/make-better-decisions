package pyerr

import (
	"bytes"
	"encoding/hex"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestNewOSError(t *testing.T) {
	cases := []struct {
		err        error
		filenames  []string
		class, msg string
	}{
		{syscall.ENOENT, []string{"/tmp/x"}, "FileNotFoundError", "[Errno 2] No such file or directory: '/tmp/x'"},
		{syscall.EXDEV, []string{"a", "b"}, "OSError", "[Errno 18] Cross-device link: 'a' -> 'b'"},
		{syscall.EACCES, nil, "PermissionError", "[Errno 13] Permission denied"},
		{&fs.PathError{Op: "open", Path: "p", Err: syscall.EISDIR}, []string{"it's"}, "IsADirectoryError",
			`[Errno 21] Is a directory: "it's"`},
		{errors.New("short write"), []string{"ignored"}, "OSError", "short write"},
	}
	for _, c := range cases {
		got := NewOSError(c.err, c.filenames...)
		if got.PyType() != c.class || got.Error() != c.msg || !errors.Is(got, c.err) {
			t.Errorf("NewOSError(%v, %q) = %s %q", c.err, c.filenames, got.PyType(), got.Error())
		}
	}
	var target *OSError
	if !errors.As(error(NewOSError(syscall.EPIPE)), &target) || target.Class != "BrokenPipeError" {
		t.Errorf("errors.As(EPIPE) = %v", target)
	}
}

func TestError(t *testing.T) {
	err := error(&Error{Class: "ValueError", Msg: "embedded null byte"})
	if err.Error() != "embedded null byte" || err.(*Error).PyType() != "ValueError" {
		t.Errorf("Error = %s %q", err.(*Error).PyType(), err)
	}
}

// oracleScript prints [class, str] of OSError(n, os.strerror(n), "f") for every errno the errno module
// names, plus 0 and a few unnamed numbers past the last.
const oracleScript = `
import errno, json, os
last = max(errno.errorcode)
print(json.dumps({n: [type(e).__name__, str(e)] for n in range(last + 4)
                  for e in [OSError(n, os.strerror(n), "f")]}))
`

func TestErrnosMatchPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	output, err := exec.Command(python, "-I", "-c", oracleScript).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want map[int][2]string
	if err := stdjson.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	for n, pair := range want {
		got := NewOSError(syscall.Errno(n), "f")
		if got.PyType() != pair[0] || got.Error() != pair[1] {
			t.Errorf("errno %d: Go %s %q; Python %s %q", n, got.PyType(), got.Error(), pair[0], pair[1])
		}
	}
	t.Logf("%d errnos compared", len(want))
}

// TestEncodeMatchesPython compares EncodeUTF8, Fsencode and EncodeBackslashReplace with str.encode("utf-8")
// under the strict, surrogateescape and backslashreplace handlers, on strings of lone surrogates (WTF-8), raw bytes and ordinary characters.
func TestEncodeMatchesPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	// Each piece is a Go string and the code point Python sees for it.
	pieces := []struct {
		text string
		code rune
	}{
		{"a", 'a'}, {"é", 0xE9}, {"\U0001F600", 0x1F600},
		{"\xed\xa0\x80", 0xD800}, {"\xed\xaf\xbf", 0xDBFF}, {"\xed\xb0\x80", 0xDC00},
		{"\xed\xb2\x80", 0xDC80}, {"\xed\xb3\xbf", 0xDCFF}, {"\xed\xbf\xbf", 0xDFFF},
		{"\x80", 0xDC80}, {"\xff", 0xDCFF},
	}
	state := uint64(1)
	cases := [][2]any{} // Go text, Python code points
	for i := 0; i < 3000; i++ {
		var text string
		codes := []rune{}
		for j := 0; j < i%7; j++ {
			state = state*6364136223846793005 + 1442695040888963407
			piece := pieces[(state>>33)%uint64(len(pieces))]
			text += piece.text
			codes = append(codes, piece.code)
		}
		cases = append(cases, [2]any{text, codes})
	}
	codes := make([][]rune, len(cases))
	for i, c := range cases {
		codes[i] = c[1].([]rune)
	}
	input, _ := stdjson.Marshal(codes)
	script := `import json, sys
out = []
for codes in json.load(sys.stdin):
    s = "".join(map(chr, codes))
    row = []
    for handler in ("strict", "surrogateescape", "backslashreplace"):
        try:
            row.append(["ok", s.encode("utf-8", handler).hex()])
        except UnicodeEncodeError as error:
            row.append(["error", str(error)])
    out.append(row)
print(json.dumps(out))`
	command := exec.Command(python, "-I", "-c", script)
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var want [][3][2]string
	if err := stdjson.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		backslashReplace := func(s string) (string, error) { return EncodeBackslashReplace(s), nil }
		for j, encode := range []func(string) (string, error){EncodeUTF8, Fsencode, backslashReplace} {
			got := [2]string{"ok", ""}
			if encoded, err := encode(c[0].(string)); err != nil {
				got = [2]string{"error", err.Error()}
			} else {
				got[1] = hex.EncodeToString([]byte(encoded))
			}
			if got != want[i][j] {
				t.Errorf("case %q handler %d: Go %v; Python %v", c[0], j, got, want[i][j])
			}
		}
	}
}

func TestIsInstance(t *testing.T) {
	Register("RequestErrorForTest", "ValueError")
	cases := []struct {
		err   error
		class string
		want  bool
	}{
		{&Error{Class: "JSONDecodeError"}, "ValueError", true},
		{&Error{Class: "UnicodeDecodeError"}, "ValueError", true},
		{&Error{Class: "UnicodeEncodeError"}, "UnicodeError", true},
		{&Error{Class: "RequestErrorForTest"}, "ValueError", true},
		{&Error{Class: "RequestErrorForTest"}, "Exception", true},
		{&Error{Class: "ValueError"}, "RequestErrorForTest", false},
		{&Error{Class: "TypeError"}, "ValueError", false},
		{&OSError{Class: "BrokenPipeError"}, "OSError", true},
		{&OSError{Class: "TimeoutError"}, "TimeoutExpired", false},
		{fmt.Errorf("wrapped: %w", &Error{Class: "KeyError"}), "LookupError", true},
		{errors.New("plain Go error"), "Exception", false},
		{nil, "Exception", false},
	}
	for _, c := range cases {
		if got := IsInstance(c.err, c.class); got != c.want {
			t.Errorf("IsInstance(%v, %s) = %v, want %v", c.err, c.class, got, c.want)
		}
	}
}

// TestBasesMatchPython compares the base of every builtin and stdlib class in the table with CPython.
func TestBasesMatchPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	script := `import builtins, json, subprocess
classes = dict(vars(builtins), JSONDecodeError=json.JSONDecodeError, SubprocessError=subprocess.SubprocessError,
               TimeoutExpired=subprocess.TimeoutExpired)
print(json.dumps({name: [b.__name__ for b in c.__bases__] for name, c in classes.items()
                  if isinstance(c, type) and issubclass(c, BaseException)}))`
	output, err := exec.Command(python, "-I", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := stdjson.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	for class, base := range bases {
		if class == "RequestErrorForTest" {
			continue
		}
		wantBases, ok := want[class]
		if !ok {
			t.Errorf("%s is not a Python builtin or table class", class)
			continue
		}
		wantBase := strings.Join(wantBases, ", ")
		if wantBase == "object" {
			wantBase = ""
		}
		if base != wantBase {
			t.Errorf("base of %s = %q, want %q", class, base, wantBase)
		}
	}
}

func TestWriteUncaught(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&Error{Class: "ValueError"}, "ValueError\n"},
		{&Error{Class: "InstallError", Msg: "bad", Module: "agent_decisions.install"}, "agent_decisions.install.InstallError: bad\n"},
		{&OSError{Class: "FileNotFoundError", Msg: "[Errno 2] x"}, "FileNotFoundError: [Errno 2] x\n"},
		{&Error{Class: "RuntimeError", Msg: "a\xed\xa0\x80b\xff", Module: "agent_decisions.runtime"},
			`agent_decisions.runtime.RuntimeError: a\ud800b\udcff` + "\n"},
		{errors.New("go"), "Exception: go\n"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		WriteUncaught(&out, c.err)
		if out.String() != c.want {
			t.Errorf("WriteUncaught(%v) = %q, want %q", c.err, out.String(), c.want)
		}
	}
}
