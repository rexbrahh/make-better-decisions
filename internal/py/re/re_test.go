package re

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"testing"
)

var classes = []struct{ escape, class string }{
	{`\s`, Space}, {`\S`, NotSpace}, {`\w`, Word}, {`\W`, NotWord}, {`\d`, Digit}, {`\D`, NotDigit},
}

func TestClasses(t *testing.T) {
	cases := []struct {
		class, text string
		want        bool
	}{
		{Space, "\v", true}, {Space, "\x1c", true}, {Space, "\u3000", true}, {Space, "\u200b", false},
		{NotSpace, "\n", false}, {NotSpace, "\u200b", true},
		{Word, "é", true}, {Word, "_", true}, {Word, "Ⅷ", true}, {Word, "-", false},
		{NotWord, "\n", true}, {NotWord, "é", false},
		{Digit, "٣", true}, {Digit, "²", false}, {NotDigit, "²", true}, {NotDigit, "7", false},
	}
	for _, c := range cases {
		if got := regexp.MustCompile(`\A` + c.class + `\z`).MatchString(c.text); got != c.want {
			t.Errorf("%.12s... on %q = %v; want %v", c.class, c.text, got, c.want)
		}
	}
}

// TestClassesMatchPython matches every code point but the surrogates, which Go strings cannot hold as
// text regexp reads, against each class and against Python's re.fullmatch of the escape it replaces.
func TestClassesMatchPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to run oracle tests")
	}
	script := `
import json, re, sys
assert sys.version_info[:2] == (3, 15), sys.version
result = {}
for escape in sys.argv[1:]:
    pattern, found = re.compile(escape), []
    for cp in range(0x110000):
        if not 0xD800 <= cp <= 0xDFFF and pattern.fullmatch(chr(cp)):
            if found and found[-1][1] == cp - 1:
                found[-1][1] = cp
            else:
                found.append([cp, cp])
    result[escape] = found
json.dump(result, sys.stdout)
`
	args := []string{"-I", "-c", script}
	for _, c := range classes {
		args = append(args, c.escape)
	}
	output, err := exec.Command(python, args...).Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("python: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	var want map[string][][2]rune
	if err := json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	for _, c := range classes {
		re := regexp.MustCompile(`\A` + c.class + `\z`)
		var got [][2]rune
		for r := rune(0); r <= 0x10ffff; r++ {
			if r >= 0xd800 && r <= 0xdfff || !re.MatchString(string(r)) {
				continue
			}
			if n := len(got); n > 0 && got[n-1][1] == r-1 {
				got[n-1][1] = r
			} else {
				got = append(got, [2]rune{r, r})
			}
		}
		if expected := want[c.escape]; !slices.Equal(got, expected) {
			i := 0
			for i < min(len(got), len(expected)) && got[i] == expected[i] {
				i++
			}
			t.Errorf("the class for %s differs from Python at range %d: %x; Python %x",
				c.escape, i, got[i:min(i+1, len(got))], expected[i:min(i+1, len(expected))])
		}
	}
}
