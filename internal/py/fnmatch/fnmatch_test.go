package fnmatch

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFnmatchCase(t *testing.T) {
	cases := []struct {
		name, pattern string
		want          bool
	}{
		{"abc", "a*c", true}, {"abc", "a?c", true}, {"a\nc", "a?c", true}, {"", "*", true}, {"", "", true},
		{"a", "", false}, {"abcbc", "*bc", true}, {"aXbYc", "a*b*c", true}, {"ab", "a*?b", false},
		{"a/b", "a*b", true}, {".x", "*x", true},
		// An unclosed "[" is literal, and a backslash is an ordinary character.
		{"[", "[", true}, {"[a", "[a", true}, {"a", "[a", false}, {`\`, `\`, true}, {`\`, `[\]`, true},
		{"a", `\a`, false}, {"[!]", "[!]", true}, {"!", "[!]", false},
		// A "]" right after "[" or "[!" is in the set.
		{"]", "[]]", true}, {"a", "[]a]", true}, {"]", "[!]]", false}, {"a", "[!]]", true}, {"b", "[!]-a]", true},
		// Ranges, and hyphens that do not make one.
		{"b", "[a-c]", true}, {"-", "[a-]", true}, {"-", "[-a]", true}, {"é", "[à-ê]", true},
		{"d", "[a-c-e]", false}, {"e", "[a-c-e]", true}, {"-", "[a-c-e]", true},
		{"-", "[a-b-c]", true}, {"c", "[a-b-z]", false},
		// CPython removes a reversed range; what is left can be empty, or start with "!" and so be negated.
		{"b", "[c-a]", false}, {"x", "[!c-a]", true}, {"-", "[c-b-a]", true}, {"a", "[c-b-a]", true}, {"b", "[c-b-a]", false},
		{"-", "[z-a!-x]", false}, {"x", "[z-a!-x]", false}, {"y", "[z-a!-x]", true}, {"!", "[z-a!-x]", true},
		// Characters that the regular expression set escapes are literal.
		{"^", "[^]", true}, {"&", "[&&]", true},
		// A surrogate and an undecodable byte are one code point each.
		{"\xed\xa0\x80", "?", true}, {"\xff", "?", true}, {"\xff", "[\xff]", true},
	}
	for _, c := range cases {
		if got := FnmatchCase(c.name, c.pattern); got != c.want {
			t.Errorf("FnmatchCase(%q, %q) = %v; want %v", c.name, c.pattern, got, c.want)
		}
	}
}

// alphabet holds the pieces random names and patterns are made of: every pattern metacharacter, path
// characters, characters on either side of the metacharacters in code point order, non-ASCII code points,
// a lone surrogate and an undecodable byte.
var alphabet = []string{
	"*", "?", "[", "]", "!", "-", `\`, "/", ".", "^", "&", "a", "b", "z", "A", "\n", " ",
	"é", "ß", "Σ", "😀", "\xed\xa0\x80", "\xff",
}

// setAlphabet holds the pieces of random set bodies. It favours "-", "!" and "]", which decide how CPython
// cuts a set into ranges.
var setAlphabet = []string{"-", "-", "-", "!", "!", "]", "a", "b", "z", "^", `\`, "é", "😀"}

func randomPiece(random *rand.Rand) string {
	return alphabet[random.IntN(len(alphabet))]
}

// randomSet returns a set made of random pieces and, about a quarter of the time each, piece.
func randomSet(random *rand.Rand, piece string) string {
	body := ""
	for range 1 + random.IntN(7) {
		if random.IntN(4) == 0 {
			body += piece
		} else {
			body += setAlphabet[random.IntN(len(setAlphabet))]
		}
	}
	return "[" + body + "]"
}

// randomPattern returns a pattern made from name, so that many pairs match: it keeps, replaces, wraps in
// a set, adds a star before, or swaps for a random set each piece of name, or it returns a fully random pattern.
func randomPattern(random *rand.Rand, name []string) string {
	var b strings.Builder
	if random.IntN(3) == 0 {
		for range random.IntN(10) {
			b.WriteString(randomPiece(random))
		}
		return b.String()
	}
	for _, piece := range name {
		switch random.IntN(9) {
		case 0:
			b.WriteString("?")
		case 1:
			b.WriteString("*" + piece)
		case 2:
			b.WriteString("[" + piece + randomPiece(random) + "]")
		case 3:
			b.WriteString("[!" + randomPiece(random) + "]")
		case 4:
			b.WriteString("[" + randomPiece(random) + "-" + randomPiece(random) + "]")
		case 5:
			b.WriteString(randomPiece(random))
		case 6:
			b.WriteString(randomSet(random, piece))
		default:
			b.WriteString(piece)
		}
	}
	return b.String()
}

// TestFnmatchCaseMatchesPython compares FnmatchCase with fnmatch.fnmatchcase on 20,000 random pairs.
func TestFnmatchCaseMatchesPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to run oracle tests")
	}
	random := rand.New(rand.NewPCG(3, 4))
	type pair struct{ name, pattern string }
	var pairs []pair
	var request bytes.Buffer
	for range 20000 {
		var name []string
		for range random.IntN(7) {
			name = append(name, randomPiece(random))
		}
		p := pair{strings.Join(name, ""), randomPattern(random, name)}
		pairs = append(pairs, p)
		request.WriteString(hex.EncodeToString([]byte(p.name)) + " " + hex.EncodeToString([]byte(p.pattern)) + "\n")
	}
	// wtf8 decodes the Go strings: a 3-byte surrogate is that surrogate and any other bad byte b is U+DC00+b.
	script := `
import codecs, fnmatch, sys, warnings
assert sys.version_info[:2] == (3, 15), sys.version
warnings.simplefilter("ignore")

def wtf8(error):
    data, at = error.object, error.start
    if data[at] == 0xED and len(data) > at + 2 and 0xA0 <= data[at + 1] <= 0xBF and 0x80 <= data[at + 2] <= 0xBF:
        return chr(0xD000 | (data[at + 1] & 0x3F) << 6 | data[at + 2] & 0x3F), at + 3
    return chr(0xDC00 + data[at]), at + 1

codecs.register_error("wtf8", wtf8)
for line in sys.stdin:
    name, pattern = (bytes.fromhex(part).decode("utf-8", "wtf8") for part in line.split(" "))
    print(int(fnmatch.fnmatchcase(name, pattern)))
`
	command := exec.Command(python, "-I", "-c", script)
	command.Stdin = &request
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("python: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	checked, matched := 0, 0
	for ; scanner.Scan(); checked++ {
		p, want := pairs[checked], scanner.Text() == "1"
		if got := FnmatchCase(p.name, p.pattern); got != want {
			t.Errorf("FnmatchCase(%q, %q) = %v; Python %v", p.name, p.pattern, got, want)
		}
		if want {
			matched++
		}
	}
	if checked != len(pairs) {
		t.Fatalf("Python answered %d of %d pairs", checked, len(pairs))
	}
	t.Logf("%d pairs, %d match", checked, matched)
}
