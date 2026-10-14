package pathlib

import (
	stdjson "encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
)

func TestStr(t *testing.T) {
	for p, want := range map[string]string{
		"//a/./b//": "//a/b", "///a/../b/": "/a/../b", "./x/.": "x", ".": ".", "/": "/", "//": "//", "a//b": "a/b",
		"": ".",
	} {
		if got := Str(p); got != want {
			t.Errorf("Str(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestParentNameJoin(t *testing.T) {
	for _, c := range []struct{ p, parent, name, joined string }{
		{".", ".", "", "n"},
		{"/", "/", "", "/n"},
		{"//", "//", "", "//n"},
		{"//a", "//", "a", "//a/n"},
		{"/a", "/", "a", "/a/n"},
		{"a", ".", "a", "a/n"},
		{"a/..", "a", "..", "a/../n"},
		{"/x/y/z", "/x/y", "z", "/x/y/z/n"},
	} {
		if got := Parent(c.p); got != c.parent {
			t.Errorf("Parent(%q) = %q, want %q", c.p, got, c.parent)
		}
		if got := Name(c.p); got != c.name {
			t.Errorf("Name(%q) = %q, want %q", c.p, got, c.name)
		}
		if got := Join(c.p, "n"); got != c.joined {
			t.Errorf("Join(%q, n) = %q, want %q", c.p, got, c.joined)
		}
	}
}

// TestAbsolute covers the working directory "/", where joining with a second "/" would spell the root "//".
func TestAbsolute(t *testing.T) {
	t.Chdir("/")
	for p, want := range map[string]string{"x": "/x", "a/b": "/a/b", ".": "/", "/y": "/y", "//y": "//y"} {
		if got, err := Absolute(p); got != want || err != nil {
			t.Errorf("Absolute(%q) = %q, %v, want %q", p, got, err, want)
		}
	}
}

func TestMkdir(t *testing.T) {
	root := t.TempDir()
	deep := Join(root, "a/b/c")
	if err := Mkdir(deep, 0o700, false); errorClass(err) != "FileNotFoundError" ||
		err.Error() != "[Errno 2] No such file or directory: '"+deep+"'" {
		t.Fatalf("Mkdir without parents = %v", err)
	}
	if err := Mkdir(deep, 0o700, true); err != nil {
		t.Fatal(err)
	}
	if err := Mkdir(deep, 0o700, true); err != nil {
		t.Fatalf("exist_ok: %v", err)
	}
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	for path, mode := range map[string]os.FileMode{Join(root, "a"): 0o777, deep: 0o700} {
		info, err := os.Stat(path)
		if want := mode &^ os.FileMode(mask); err != nil || info.Mode().Perm() != want {
			t.Errorf("mode of %s = %v (%v), want %v", path, info.Mode().Perm(), err, want)
		}
	}
	file := Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Mkdir(file, 0o700, true); errorClass(err) != "FileExistsError" {
		t.Errorf("Mkdir over a file = %v", err)
	}
	if err := Mkdir(Join(file, "x"), 0o700, true); errorClass(err) != "NotADirectoryError" {
		t.Errorf("Mkdir under a file = %v", err)
	}
}

func errorClass(err error) string {
	var osError *pyerr.OSError
	if errors.As(err, &osError) {
		return osError.Class
	}
	return "not an OSError"
}

// oracleScript prints [str(p), str(p.parent), p.name, str(p / "n")] for each PurePosixPath(p) on stdin.
const oracleScript = `
import json, sys
from pathlib import PurePosixPath
for line in sys.stdin:
    p = PurePosixPath(json.loads(line))
    print(json.dumps([str(p), str(p.parent), p.name, str(p / "n")]))
`

func TestPathsMatchPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	pieces := []string{"", "/", "//", "///", ".", "..", "a", "é", "./", "b/"}
	var paths []string
	for _, x := range pieces {
		for _, y := range pieces {
			for _, z := range pieces {
				paths = append(paths, x+y+z)
			}
		}
	}
	var input strings.Builder
	for _, p := range paths {
		line, _ := stdjson.Marshal(p)
		input.Write(line)
		input.WriteByte('\n')
	}
	command := exec.Command(python, "-I", "-c", oracleScript)
	command.Stdin = strings.NewReader(input.String())
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != len(paths) {
		t.Fatalf("python answered %d of %d paths", len(lines), len(paths))
	}
	for i, p := range paths {
		var want [4]string
		if err := stdjson.Unmarshal([]byte(lines[i]), &want); err != nil {
			t.Fatal(err)
		}
		s := Str(p)
		if got := [4]string{s, Parent(s), Name(s), Join(s, "n")}; got != want {
			t.Errorf("%q: Go %q; Python %q", p, got, want)
		}
	}
	t.Logf("%d paths compared", len(paths))
}

// TestReadText checks the texts and errors of Path.read_text() in CPython 3.15, which encodes the path with
// os.fsencode before it opens the file.
func TestReadText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ok", []byte("a\r\nb\rc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/bad", []byte("a\r\nb\rc\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadText(dir + "/ok"); got != "a\nb\nc\n" || err != nil {
		t.Errorf("ReadText(ok) = %q, %v", got, err)
	}
	for path, want := range map[string]string{
		dir + "/bad":    "UnicodeDecodeError 'utf-8' codec can't decode byte 0xff in position 6: invalid start byte",
		"a\x00b":        "ValueError embedded null byte",
		"x\xed\xa0\x80": `UnicodeEncodeError 'utf-8' codec can't encode character '\ud800' in position 1: surrogates not allowed`,
		dir:             "IsADirectoryError [Errno 21] Is a directory: '" + dir + "'",
		dir + "/none":   "FileNotFoundError [Errno 2] No such file or directory: '" + dir + "/none'",
	} {
		_, err := ReadText(path)
		if err == nil {
			t.Errorf("ReadText(%q) succeeded, want %s", path, want)
		} else if got := pyerr.Class(err) + " " + err.Error(); got != want {
			t.Errorf("ReadText(%q) error = %s, want %s", path, got, want)
		}
	}
}
