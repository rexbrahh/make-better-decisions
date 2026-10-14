package ospath

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandUser(t *testing.T) {
	cases := []struct{ path, home, want string }{
		{"~", "/Users/x", "/Users/x"},
		{"~/a", "/Users/x/", "/Users/x/a"},
		{"~", "/", "/"},
		{"~/a", "/", "/a"},
		{"~", "", "/"},
		{"a/~", "/Users/x", "a/~"},
		{"~nosuchuser/a", "/Users/x", "~nosuchuser/a"},
	}
	for _, c := range cases {
		t.Setenv("HOME", c.home)
		if got, err := ExpandUser(c.path); err != nil || got != c.want {
			t.Errorf("ExpandUser(%q) with HOME=%q = %q, %v; want %q", c.path, c.home, got, err, c.want)
		}
	}
	if _, err := ExpandUser("~a\x00b/c"); err == nil {
		t.Error("ExpandUser accepted a NUL byte in the user name; Python raises ValueError")
	}
}

// TestExpandUserMatchesPython compares ExpandUser with os.path.expanduser, with and without HOME.
func TestExpandUserMatchesPython(t *testing.T) {
	python := oracle(t)
	paths := []string{"~", "~/", "~/a", "~root", "~root/", "~nobody/x", "~_www", "~nosuchuser/x", "~~", "x~", ""}
	check := func(label string) {
		want := runOracle(t, python, "os.path.expanduser", paths)
		for i, path := range paths {
			if got, err := ExpandUser(path); err != nil || got != want[i] {
				t.Errorf("%s: ExpandUser(%q) = %q, %v; Python %q", label, path, got, err, want[i])
			}
		}
	}
	for _, home := range []string{"", "/", "/h/", "/h//"} {
		t.Setenv("HOME", home)
		check("HOME=" + home)
	}
	t.Setenv("HOME", "") // restores HOME after the test
	os.Unsetenv("HOME")
	check("HOME unset")
}

// oracle returns the CPython named by AGENT_DECISIONS_PYTHON, or skips the test.
func oracle(t *testing.T) string {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to run oracle tests")
	}
	return python
}

// runOracle applies the os.path function to each path in one CPython process with this process's environment.
func runOracle(t *testing.T, python string, function string, paths []string) []string {
	script := "import json, os, sys\nprint(json.dumps([" + function + "(p) for p in json.load(sys.stdin)]))"
	command := exec.Command(python, "-I", "-c", script)
	input, _ := json.Marshal(paths)
	command.Stdin = strings.NewReader(string(input))
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatal(err)
	}
	return results
}

// TestRealpathMatchesPython compares Realpath with os.path.realpath, strict and not, on a tree of symlinks,
// for absolute and relative paths (from the same working directory) and paths with a NUL.
func TestRealpathMatchesPython(t *testing.T) {
	python := oracle(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.MkdirAll(filepath.Join(root, "d/e"), 0o700))
	must(t, os.WriteFile(filepath.Join(root, "d/f"), nil, 0o600))
	links := map[string]string{
		"abs": root + "/d", "rel": "d/e", "chain": "rel", "up": "../d", "dangling": "missing/x",
		"loop1": "loop2", "loop2": "loop1", "self": "self", "d/e/back": "../..", "dir_dot": "./d/./e/",
		"file": "d/f",
	}
	for name, target := range links {
		must(t, os.Symlink(target, filepath.Join(root, name)))
	}
	paths := []string{"/", "//", root, root + "/", root + "//d/./e/", root + "/d/../d/e/..", root + "/abs/e",
		root + "/rel/..", root + "/chain/back/abs", root + "/up/e", root + "/dangling", root + "/dangling/y/..",
		root + "/loop1", root + "/loop1/x", root + "/self/..", root + "/d/e/back/rel/back", root + "/dir_dot",
		root + "/missing/../abs", root + "/../../..", "/" + strings.Repeat("a/", 600),
		root + "/d/f", root + "/d/f/", root + "/d/f/x", root + "/file", root + "/file/..",
		"", ".", "d/e", "rel/../d/f", "missing", "loop1", "../" + filepath.Base(root) + "/abs",
		root + "/a\x00b", root + "/missing/a\x00b", "a\x00b/..", root + "/d/f/a\x00b"}
	t.Chdir(root)
	for _, strict := range []bool{false, true} {
		want := realpathOracle(t, python, root, paths, strict)
		for i, path := range paths {
			got, err := Realpath(path, strict)
			outcome := "ok:" + got
			if err != nil {
				outcome = err.(interface{ PyType() string }).PyType() + ": " + err.Error()
			}
			if outcome != want[i] {
				t.Errorf("Realpath(%q, %v) = %q; Python gives %q", strings.TrimPrefix(path, root), strict, outcome, want[i])
			}
		}
	}
}

// realpathOracle returns "ok:" + os.path.realpath(path, strict=strict), or "Class: message" for the exception
// it raises, for each path, from a CPython process whose working directory is dir.
func realpathOracle(t *testing.T, python, dir string, paths []string, strict bool) []string {
	script := `import json, os, sys
def outcome(path, strict):
    try:
        return "ok:" + os.path.realpath(path, strict=strict)
    except Exception as error:
        return type(error).__name__ + ": " + str(error)
paths, strict = json.load(sys.stdin)
print(json.dumps([outcome(path, strict) for path in paths]))`
	command := exec.Command(python, "-I", "-c", script)
	command.Dir = dir
	input, _ := json.Marshal([]any{paths, strict})
	command.Stdin = strings.NewReader(string(input))
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatal(err)
	}
	return results
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
