package str

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestDecodeRune(t *testing.T) {
	cases := []struct {
		s    string
		r    rune
		size int
	}{
		{"", 0xfffd, 0}, {"a", 'a', 1}, {"é", 'é', 2}, {"\ufffd", 0xfffd, 3}, {"😀", 0x1f600, 4},
		{"\xed\xa0\x80", 0xd800, 3}, {"\xed\xbf\xbfx", 0xdfff, 3}, {"\xed\x9f\xbf", 0xd7ff, 3},
		{"\xed\xa0", 0xdced, 1}, {"\x80", 0xdc80, 1}, {"\xff", 0xdcff, 1}, {"\xf0\x90\x41", 0xdcf0, 1},
	}
	for _, c := range cases {
		if r, size := DecodeRune(c.s); r != c.r || size != c.size {
			t.Errorf("DecodeRune(%q) = %#x, %d; want %#x, %d", c.s, r, size, c.r, c.size)
		}
	}
}

func TestLenAndSlice(t *testing.T) {
	s := "aé\xed\xa0\x80\xff😀" // five code points
	if got := Len(s); got != 5 {
		t.Errorf("Len(%q) = %d; want 5", s, got)
	}
	cases := []struct {
		start, stop int
		want        string
	}{
		{0, 5, s}, {1, 3, "é\xed\xa0\x80"}, {-2, 99, "\xff😀"}, {-99, 1, "a"}, {3, 2, ""}, {5, 9, ""}, {-1, -1, ""},
	}
	for _, c := range cases {
		if got := Slice(s, c.start, c.stop); got != c.want {
			t.Errorf("Slice(%q, %d, %d) = %q; want %q", s, c.start, c.stop, got, c.want)
		}
	}
	if got := SliceFrom(s, -1); got != "😀" {
		t.Errorf("SliceFrom(-1) = %q", got)
	}
	if got := SliceTo(s, 2); got != "aé" {
		t.Errorf("SliceTo(2) = %q", got)
	}
}

func TestWhitespace(t *testing.T) {
	if got := Strip("\u3000\x1c a b\v\u0085"); got != "a b" {
		t.Errorf("Strip = %q", got)
	}
	if got := LStrip(" \u200ba "); got != "\u200ba " { // U+200B is not whitespace
		t.Errorf("LStrip = %q", got)
	}
	if got := RStrip("a\xed\xa0\x80\u00a0"); got != "a\xed\xa0\x80" {
		t.Errorf("RStrip = %q", got)
	}
	if got := Split("\u2028a\x1fb\u00a0\u00a0c "); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("Split = %q", got)
	}
	if got := Split(" \t"); len(got) != 0 {
		t.Errorf("Split(blank) = %q", got)
	}
	cases := map[string][]string{
		"":                    nil,
		"a\n":                 {"a"},
		"\n\n":                {"", ""},
		"a\r\nb\rc\u2028d":    {"a", "b", "c", "d"},
		"a\r\r\nb\u0085\x1ec": {"a", "", "b", "", "c"},
		"a\u0085b\u200bc":     {"a", "b\u200bc"},
	}
	for s, want := range cases {
		if got := SplitLines(s); !slices.Equal(got, want) {
			t.Errorf("SplitLines(%q) = %q; want %q", s, got, want)
		}
	}
}

func TestStripChars(t *testing.T) {
	cases := []struct{ s, chars, strip, left, right string }{
		{"xxaxx", "x", "a", "axx", "xxa"},
		{"éaé", "é", "a", "aé", "éa"},
		{"\xed\xa0\x80a\xed\xa0\x80", "\xed\xa0\x80", "a", "a\xed\xa0\x80", "\xed\xa0\x80a"},
		{"Āa", "\x80", "Āa", "Āa", "Āa"}, // the byte 0x80 inside U+0100 is not U+DC80
		{"abc", "", "abc", "abc", "abc"},
	}
	for _, c := range cases {
		if got := StripChars(c.s, c.chars); got != c.strip {
			t.Errorf("StripChars(%q, %q) = %q; want %q", c.s, c.chars, got, c.strip)
		}
		if got := LStripChars(c.s, c.chars); got != c.left {
			t.Errorf("LStripChars(%q, %q) = %q; want %q", c.s, c.chars, got, c.left)
		}
		if got := RStripChars(c.s, c.chars); got != c.right {
			t.Errorf("RStripChars(%q, %q) = %q; want %q", c.s, c.chars, got, c.right)
		}
	}
}

func TestCase(t *testing.T) {
	cases := []struct{ s, lower, casefold string }{
		{"Straße", "straße", "strasse"},
		{"İ", "i\u0307", "i\u0307"},
		{"ΟΔΟΣ ΣΑ Σ", "οδος σα σ", "οδοσ σα σ"},
		{"ΑΣ'", "ας'", "ασ'"},    // an apostrophe is case-ignorable
		{"ΑΣ'Α", "ασ'α", "ασ'α"}, // a cased letter after the ignorable one
		{"'Σ", "'σ", "'σ"},       // no cased letter before
		{"ﬃ K", "ﬃ k", "ffi k"},  // U+FB03 and the Kelvin sign
		{"a\xed\xa0\x80\xffB", "a\xed\xa0\x80\xffb", "a\xed\xa0\x80\xffb"},
	}
	for _, c := range cases {
		if got := Lower(c.s); got != c.lower {
			t.Errorf("Lower(%q) = %q; want %q", c.s, got, c.lower)
		}
		if got := Casefold(c.s); got != c.casefold {
			t.Errorf("Casefold(%q) = %q; want %q", c.s, got, c.casefold)
		}
	}
}

func TestPredicates(t *testing.T) {
	for s, want := range map[string]bool{"": false, "a1": true, "Ⅷ½": true, "a_": false, "é": true, "a b": false} {
		if got := IsAlnum(s); got != want {
			t.Errorf("IsAlnum(%q) = %v", s, got)
		}
	}
	for r, want := range map[rune]bool{' ': true, '\u3000': false, 'é': true, '\u00ad': false, 0xd800: false} {
		if got := IsPrintable(r); got != want {
			t.Errorf("IsPrintable(%#x) = %v", r, got)
		}
	}
	for r, want := range map[rune]bool{'\v': true, 0x1c: true, 0x85: true, 0x200b: false, 0xfeff: false} {
		if got := IsSpace(r); got != want {
			t.Errorf("IsSpace(%#x) = %v", r, got)
		}
	}
}

func TestRepr(t *testing.T) {
	cases := map[string]string{
		"":                           `''`,
		"a'b":                        `"a'b"`,
		`a'"`:                        `'a\'"'`,
		`a"\`:                        `'a"\\'`,
		"\t\n\r\x00\x1f\x7f":         `'\t\n\r\x00\x1f\x7f'`,
		"\u0080\u00a0\u00adé":        `'\x80\xa0\xadé'`,
		"\u2028\ufeff\uffff一":        `'\u2028\ufeff\uffff一'`,
		"\U0001f600\U000e0001\ufffd": "'😀\\U000e0001\ufffd'",
		"\xed\xa0\x80\xff":           `'\ud800\udcff'`,
	}
	for s, want := range cases {
		if got := Repr(s); got != want {
			t.Errorf("Repr(%q) = %s; want %s", s, got, want)
		}
	}
}

// oracle returns the CPython named by AGENT_DECISIONS_PYTHON, or skips the test.
func oracle(t *testing.T) string {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to run oracle tests")
	}
	return python
}

// oraclePrelude checks the interpreter version and defines dec and enc, which move a str to and from hex
// of the Go string that holds it.
const oraclePrelude = `
import codecs, json, sys
assert sys.version_info[:2] == (3, 15), sys.version

def wtf8(error):
    data, at = error.object, error.start
    if data[at] == 0xED and len(data) > at + 2 and 0xA0 <= data[at + 1] <= 0xBF and 0x80 <= data[at + 2] <= 0xBF:
        return chr(0xD000 | (data[at + 1] & 0x3F) << 6 | data[at + 2] & 0x3F), at + 3
    return chr(0xDC00 + data[at]), at + 1

codecs.register_error("wtf8", wtf8)
dec = lambda text: bytes.fromhex(text).decode("utf-8", "wtf8")
enc = lambda s: s.encode("utf-8", "surrogatepass").hex()
`

func runPython(t *testing.T, script string, input []byte) []byte {
	command := exec.Command(oracle(t), "-I", "-c", oraclePrelude+script)
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("python: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	return output
}

// wtf8 returns the Go string for code point r, with a surrogate in its 3-byte WTF-8 form.
func wtf8(r rune) string {
	if r >= 0xd800 && r <= 0xdfff {
		return string([]byte{0xed, byte(0x80 | r>>6&0x3f), byte(0x80 | r&0x3f)})
	}
	return string(r)
}

// codePoints returns the code points of s, so results that hold a surrogate as WTF-8 and as a raw byte compare equal.
func codePoints(s string) []rune {
	var runes []rune
	for s != "" {
		r, size := DecodeRune(s)
		runes = append(runes, r)
		s = s[size:]
	}
	return runes
}

func unhex(t *testing.T, text string) string {
	data, err := hex.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// spans returns the ranges of code points where holds is true.
func spans(holds func(r rune) bool) [][2]rune {
	var found [][2]rune
	for r := rune(0); r <= 0x10ffff; r++ {
		if !holds(r) {
			continue
		}
		if n := len(found); n > 0 && found[n-1][1] == r-1 {
			found[n-1][1] = r
		} else {
			found = append(found, [2]rune{r, r})
		}
	}
	return found
}

// TestCodePointsMatchPython checks every code point: the predicates, a line break inside "a?b",
// lower(), casefold() and repr() of the one-character str.
func TestCodePointsMatchPython(t *testing.T) {
	script := `
def spans(holds):
    found = []
    for cp in range(0x110000):
        if holds(chr(cp)):
            if found and found[-1][1] == cp - 1:
                found[-1][1] = cp
            else:
                found.append([cp, cp])
    return found

def changed(method):
    return {str(cp): enc(method(chr(cp))) for cp in range(0x110000) if method(chr(cp)) != chr(cp)}

out = sys.stdout.buffer
out.write(json.dumps({
    "space": spans(str.isspace), "printable": spans(str.isprintable), "alnum": spans(str.isalnum),
    "linebreak": spans(lambda c: len(("a" + c + "b").splitlines()) == 2),
    "lower": changed(str.lower), "casefold": changed(str.casefold),
}).encode() + b"\n")
for cp in range(0x110000):
    out.write(repr(chr(cp)).encode() + b"\n")
`
	scanner := bufio.NewScanner(bytes.NewReader(runPython(t, script, nil)))
	scanner.Buffer(nil, 64<<20)
	scanner.Scan()
	var want struct {
		Space, Printable, Alnum, Linebreak [][2]rune
		Lower, Casefold                    map[rune]string
	}
	if err := json.Unmarshal(scanner.Bytes(), &want); err != nil {
		t.Fatal(err)
	}
	predicates := []struct {
		name  string
		want  [][2]rune
		holds func(r rune) bool
	}{
		{"IsSpace", want.Space, IsSpace},
		{"IsPrintable", want.Printable, IsPrintable},
		{"IsAlnum", want.Alnum, func(r rune) bool { return IsAlnum(wtf8(r)) }},
		{"SplitLines", want.Linebreak, func(r rune) bool { return len(SplitLines("a"+wtf8(r)+"b")) == 2 }},
	}
	for _, p := range predicates {
		if got := spans(p.holds); !slices.Equal(got, p.want) {
			i := 0
			for i < min(len(got), len(p.want)) && got[i] == p.want[i] {
				i++
			}
			t.Errorf("%s differs from Python at range %d: %x; Python %x",
				p.name, i, got[i:min(i+1, len(got))], p.want[i:min(i+1, len(p.want))])
		}
	}
	for name, mapping := range map[string]func(string) string{"Lower": Lower, "Casefold": Casefold} {
		pythonChanged := want.Lower
		if name == "Casefold" {
			pythonChanged = want.Casefold
		}
		for r := rune(0); r <= 0x10ffff; r++ {
			got, wantHex := mapping(wtf8(r)), pythonChanged[r]
			if wantHex == "" && got != wtf8(r) || wantHex != "" && got != unhex(t, wantHex) {
				t.Errorf("%s(%U) = %q; Python changes it to %q", name, r, got, unhex(t, wantHex))
			}
		}
	}
	r := rune(0)
	for ; scanner.Scan(); r++ {
		if got := Repr(wtf8(r)); got != scanner.Text() {
			t.Errorf("Repr(%U) = %s; Python %s", r, got, scanner.Text())
		}
	}
	if r != 0x110000 {
		t.Fatalf("Python gave repr() of %d code points; want %d", r, 0x110000)
	}
}

// pool holds the pieces random test strings are made of: whitespace, line breaks, quotes, characters
// with special case mappings, case-ignorable and cased marks, surrogates and undecodable bytes.
var pool = []string{
	"a", "Z", "0", "_", "-", " ", "\t", "\n", "\r", "\r\n", "\v", "\f", "\x1c", "\x1f", "\u0085", "\u00a0",
	"\u2028", "\u3000", "\u200b", "'", `"`, `\`, "\x00", "\x7f", "\u00ad", "é", "ß", "İ", "ı", "Σ", "σ", "ς",
	"\u0391", "ſ", "K", "ﬃ", "ǅ", "ΐ", "\u0307", "\u0345", "ʰ", "·", ".", "Ⅷ", "½", "٣", "😀",
	"\U000e0001", "\ufeff", "\uffff", "\xed\xa0\x80", "\xed\xbf\xbf", "\x80", "\xff", "\xc3", "\xf0\x90",
}

func randomString(random *rand.Rand, maxLen int) string {
	var b strings.Builder
	for range random.IntN(maxLen + 1) {
		switch random.IntN(10) {
		case 0:
			b.WriteString(wtf8(random.Int32N(0x10000)))
		case 1:
			b.WriteString(wtf8(0x10000 + random.Int32N(0x100000)))
		default:
			b.WriteString(pool[random.IntN(len(pool))])
		}
	}
	return b.String()
}

// TestStringsMatchPython compares every string function with CPython on 10,000 random strings.
func TestStringsMatchPython(t *testing.T) {
	type input struct {
		S      string  `json:"s"`
		Chars  string  `json:"chars"`
		Slices [][]int `json:"slices"`
	}
	random := rand.New(rand.NewPCG(1, 2))
	var inputs []input
	var request bytes.Buffer
	for range 10000 {
		s := randomString(random, 12)
		in := input{S: s, Chars: randomString(random, 3)}
		n := Len(s)
		for range 3 {
			in.Slices = append(in.Slices, []int{random.IntN(2*n+5) - n - 2, random.IntN(2*n+5) - n - 2})
		}
		inputs = append(inputs, in)
		line, _ := json.Marshal(input{S: hex.EncodeToString([]byte(s)), Chars: hex.EncodeToString([]byte(in.Chars)), Slices: in.Slices})
		request.Write(append(line, '\n'))
	}
	script := `
for line in sys.stdin:
    case = json.loads(line)
    s, chars = dec(case["s"]), dec(case["chars"])
    print(json.dumps({
        "Len": len(s), "IsAlnum": s.isalnum(),
        "Strip": enc(s.strip()), "LStrip": enc(s.lstrip()), "RStrip": enc(s.rstrip()),
        "StripChars": enc(s.strip(chars)), "LStripChars": enc(s.lstrip(chars)), "RStripChars": enc(s.rstrip(chars)),
        "Casefold": enc(s.casefold()), "Lower": enc(s.lower()), "Repr": enc(repr(s)),
        "Split": [enc(part) for part in s.split()], "SplitLines": [enc(line) for line in s.splitlines()],
        "Slice": [enc(s[a:b]) for a, b in case["slices"]],
        "SliceFrom": [enc(s[a:]) for a, _ in case["slices"]],
        "SliceTo": [enc(s[:b]) for _, b in case["slices"]],
    }))
`
	scanner := bufio.NewScanner(bytes.NewReader(runPython(t, script, request.Bytes())))
	scanner.Buffer(nil, 1<<20)
	checked := 0
	for i := 0; scanner.Scan(); i++ {
		var want struct {
			Len                                            int
			IsAlnum                                        bool
			Strip, LStrip, RStrip, StripChars, LStripChars string
			RStripChars, Casefold, Lower, Repr             string
			Split, SplitLines, Slice, SliceFrom, SliceTo   []string
		}
		if err := json.Unmarshal(scanner.Bytes(), &want); err != nil {
			t.Fatal(err)
		}
		in := inputs[i]
		s, chars := in.S, in.Chars
		if got := Len(s); got != want.Len {
			t.Errorf("Len(%q) = %d; Python %d", s, got, want.Len)
		}
		if got := IsAlnum(s); got != want.IsAlnum {
			t.Errorf("IsAlnum(%q) = %v; Python %v", s, got, want.IsAlnum)
		}
		same := func(name, got, wantHex string) {
			if wantS := unhex(t, wantHex); !slices.Equal(codePoints(got), codePoints(wantS)) {
				t.Errorf("%s(%q, chars %q) = %q; Python %q", name, s, chars, got, wantS)
			}
		}
		sameAll := func(name string, got, wantHex []string) {
			if len(got) != len(wantHex) {
				t.Errorf("%s(%q) = %q; Python gives %d parts", name, s, got, len(wantHex))
				return
			}
			for k := range got {
				same(name, got[k], wantHex[k])
			}
		}
		same("Strip", Strip(s), want.Strip)
		same("LStrip", LStrip(s), want.LStrip)
		same("RStrip", RStrip(s), want.RStrip)
		same("StripChars", StripChars(s, chars), want.StripChars)
		same("LStripChars", LStripChars(s, chars), want.LStripChars)
		same("RStripChars", RStripChars(s, chars), want.RStripChars)
		same("Casefold", Casefold(s), want.Casefold)
		same("Lower", Lower(s), want.Lower)
		same("Repr", Repr(s), want.Repr)
		sameAll("Split", Split(s), want.Split)
		sameAll("SplitLines", SplitLines(s), want.SplitLines)
		var slice, from, to []string
		for _, ab := range in.Slices {
			slice = append(slice, Slice(s, ab[0], ab[1]))
			from = append(from, SliceFrom(s, ab[0]))
			to = append(to, SliceTo(s, ab[1]))
		}
		sameAll("Slice", slice, want.Slice)
		sameAll("SliceFrom", from, want.SliceFrom)
		sameAll("SliceTo", to, want.SliceTo)
		checked++
	}
	if checked != len(inputs) {
		t.Fatalf("Python answered %d of %d cases", checked, len(inputs))
	}
}
