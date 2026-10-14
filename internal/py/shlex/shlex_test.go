package shlex

import (
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// commandInput mirrors gates.py _CommandInput: after a comment, the newline stays in the stream as a separator.
type commandInput struct{ *StringStream }

func (c commandInput) ReadLine() string {
	line := c.StringStream.ReadLine()
	if strings.HasSuffix(line, "\n") {
		if _, err := c.Seek(c.Tell()-1, io.SeekStart); err != nil {
			panic(err)
		}
	}
	return line
}

// config builds a lexer the way one caller does.
type config struct {
	name string
	new  func(command string) *Lexer
}

var configs = []config{
	{"guard", func(command string) *Lexer { // gates.py guard and the classify heredoc header
		lexer := New(command, true, DefaultPunctuation)
		lexer.WhitespaceSplit = true
		return lexer
	}},
	{"classify", func(command string) *Lexer { // gates.py classify
		lexer := NewStream(commandInput{NewStringStream(command)}, true, "();<>|&\n")
		lexer.Whitespace = " \t\r"
		lexer.WhitespaceSplit = true
		return lexer
	}},
	{"interactive", func(command string) *Lexer { // gates.py _interactive
		lexer := New(command, false, DefaultPunctuation)
		lexer.WhitespaceSplit = true
		return lexer
	}},
	{"is_decide", func(command string) *Lexer { // transcript.py is_decide
		lexer := New(command, true, "")
		lexer.WhitespaceSplit = true
		lexer.Commenters = ""
		return lexer
	}},
	{"posix", func(command string) *Lexer { return New(command, true, "") }},
	{"non_posix", func(command string) *Lexer { return New(command, false, "") }},
}

// result is what one lexer produces: the tokens before an error, the error text and the final line number.
type result struct {
	Tokens []string `json:"tokens"`
	Error  string   `json:"error"`
	Lineno int      `json:"lineno"`
}

func run(lexer *Lexer) result {
	var r result
	for {
		token, ok, err := lexer.GetToken()
		if err != nil {
			r.Error = err.Error()
			break
		}
		if !ok {
			break
		}
		r.Tokens = append(r.Tokens, token)
	}
	r.Lineno = lexer.Lineno
	return r
}

func TestConfigs(t *testing.T) {
	cases := []struct {
		config, command string
		want            result
	}{
		{"guard", `cat ~/a>b;echo "x y"&&ls`, result{[]string{"cat", "~/a", ">", "b", ";", "echo", "x y", "&&", "ls"}, "", 1}},
		{"guard", "a #c\nb", result{[]string{"a", "b"}, "", 2}},
		{"guard", `a "b`, result{[]string{"a"}, "No closing quotation", 1}},
		{"guard", `a \`, result{[]string{"a"}, "No escaped character", 1}},
		{"classify", "ts-decide x # c\nls", result{[]string{"ts-decide", "x", "\n", "ls"}, "", 3}},
		{"classify", "a\r\nb", result{[]string{"a", "\n", "b"}, "", 2}},
		{"interactive", `python3 - <<'EOF'`, result{[]string{"python3", "-", "<<", "'EOF'"}, "", 1}},
		{"is_decide", `printf '%s\n' "ts-decide verdict: x" # y`, result{[]string{"printf", `%s\n`, "ts-decide verdict: x", "#", "y"}, "", 1}},
		{"posix", `a"b c"d \e ""`, result{[]string{"ab cd", "e", ""}, "", 1}},
		{"posix", "é×", result{[]string{"é", "×"}, "", 1}},
		{"non_posix", "a;b 'c d'", result{[]string{"a", ";", "b", "'c d'"}, "", 1}},
		{"non_posix", "é", result{[]string{"é"}, "", 1}},
	}
	byName := map[string]config{}
	for _, c := range configs {
		byName[c.name] = c
	}
	for _, c := range cases {
		got := run(byName[c.config].new(c.command))
		if !slices.Equal(got.Tokens, c.want.Tokens) || got.Error != c.want.Error || got.Lineno != c.want.Lineno {
			t.Errorf("%s %q = %+v; want %+v", c.config, c.command, got, c.want)
		}
	}
}

func TestPushToken(t *testing.T) {
	lexer := New("c", true, "")
	lexer.PushToken("b")
	lexer.PushToken("")
	tokens, err := lexer.Tokens()
	if want := []string{"", "b", "c"}; err != nil || !slices.Equal(tokens, want) {
		t.Errorf("Tokens() = %q, %v; want %q", tokens, err, want)
	}
	lexer = New("c", false, "")
	lexer.PushToken("")
	if token, ok, err := lexer.GetToken(); token != "" || ok || err != nil {
		t.Errorf("non-posix GetToken() = %q, %v, %v; want the eof value", token, ok, err)
	}
}

func TestSplitJoinQuote(t *testing.T) {
	if got, err := Split(`a 'b c' "d\"e" f#g`); err != nil || !slices.Equal(got, []string{"a", "b c", `d"e`, "f#g"}) {
		t.Errorf("Split = %q, %v", got, err)
	}
	var shlexError *Error
	if _, err := Split(`'a`); !errors.As(err, &shlexError) || err.Error() != "No closing quotation" {
		t.Errorf("Split(%q) error = %v", `'a`, err)
	}
	quotes := map[string]string{"": "''", "a-b/c.d=e:f,g@h%i+j_k": "a-b/c.d=e:f,g@h%i+j_k", "a b": "'a b'",
		"it's": `'it'"'"'s'`, "é": "'é'", "~": "'~'"}
	for s, want := range quotes {
		if got := Quote(s); got != want {
			t.Errorf("Quote(%q) = %q; want %q", s, got, want)
		}
	}
	if got := Join([]string{"ls", "a b", ""}); got != "ls 'a b' ''" {
		t.Errorf("Join = %q", got)
	}
}

func TestStringStreamSeek(t *testing.T) {
	s := NewStringStream("é😀\nx")
	if line := s.ReadLine(); line != "é😀\n" || s.Tell() != 3 {
		t.Fatalf("ReadLine() = %q at %d", line, s.Tell())
	}
	if _, err := s.Seek(1, io.SeekStart); err != nil || s.Read() != "😀" || s.Tell() != 2 {
		t.Errorf("Seek(1) did not land on the second character")
	}
	if _, err := s.Seek(-1, io.SeekStart); err == nil {
		t.Error("Seek(-1) succeeded; StringIO rejects a negative position")
	}
	if _, err := s.Seek(9, io.SeekStart); err != nil || s.Read() != "" || s.Tell() != 9 {
		t.Error("Seek past the end did not read as the end")
	}
}

// oracleScript runs the same configurations in CPython. It reads a JSON list of commands and writes one JSON
// object per command.
const oracleScript = `
import io, json, shlex, sys

class CommandInput(io.StringIO):
    def readline(self, size=-1):
        line = super().readline(size)
        if line.endswith("\n"):
            self.seek(self.tell() - 1)
        return line

def configured(lexer, whitespace=None, whitespace_split=False, commenters=None):
    if whitespace is not None:
        lexer.whitespace = whitespace
    lexer.whitespace_split = whitespace_split
    if commenters is not None:
        lexer.commenters = commenters
    return lexer

configs = [
    lambda c: configured(shlex.shlex(c, posix=True, punctuation_chars=True), whitespace_split=True),
    lambda c: configured(shlex.shlex(CommandInput(c), posix=True, punctuation_chars="();<>|&\n"),
                         whitespace=" \t\r", whitespace_split=True),
    lambda c: configured(shlex.shlex(c, posix=False, punctuation_chars=True), whitespace_split=True),
    lambda c: configured(shlex.shlex(c, posix=True), whitespace_split=True, commenters=""),
    lambda c: shlex.shlex(c, posix=True),
    lambda c: shlex.shlex(c, posix=False),
]

def run(lexer):
    tokens, error = [], ""
    try:
        while (token := lexer.get_token()) != lexer.eof:
            tokens.append(token)
    except ValueError as e:
        error = str(e)
    return {"tokens": tokens, "error": error, "lineno": lexer.lineno}

out = []
for command in json.load(sys.stdin):
    try:
        split, split_error = shlex.split(command), ""
    except ValueError as e:
        split, split_error = None, str(e)
    out.append({"configs": [run(new(command)) for new in configs], "split": split, "split_error": split_error,
                "quote": shlex.quote(command), "join": shlex.join(command.split("|"))})
json.dump(out, sys.stdout)
`

type oracleResult struct {
	Configs    []result `json:"configs"`
	Split      []string `json:"split"`
	SplitError string   `json:"split_error"`
	Quote      string   `json:"quote"`
	Join       string   `json:"join"`
}

// randomCommand draws from an alphabet weighted toward the characters that change lexer state.
func randomCommand(random *rand.Rand) string {
	alphabet := []string{
		`'`, `'`, `'`, `"`, `"`, `"`, `\`, `\`, `\`, "(", ")", ";", "<", ">", "|", "|", "&", "&",
		" ", " ", " ", "\t", "\r", "\n", "\n", "#", "#", "~", "=", "-", ".", "/", "*", "?", "$", "%", "@",
		"a", "b", "c", "x", "Z", "0", "9", "_", "é", "ß", "ÿ", "À", "×", "÷", "¡", " ", "ж", "中", " ",
		"😀", "𝔘",
	}
	var b strings.Builder
	for range random.IntN(30) {
		b.WriteString(alphabet[random.IntN(len(alphabet))])
	}
	return b.String()
}

// TestMatchesPython compares every configuration, Split, Quote and Join with CPython on fixed and random
// commands. It needs AGENT_DECISIONS_PYTHON to name a Python 3.15 interpreter.
func TestMatchesPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("AGENT_DECISIONS_PYTHON is not set")
	}
	commands := []string{"", " ", "#", "\\", "'", `"`, "a\\\nb", `"a\"b\\c\$d"`, `'a\'`, "a&&b||c;;d<<<e>>f",
		"x # y\nz", "ls|&>(x)", "env -S 'a b' c", "a\x00b"}
	random := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		commands = append(commands, randomCommand(random))
	}
	input, err := json.Marshal(commands)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(python, "-I", "-c", oracleScript)
	command.Stdin = strings.NewReader(string(input))
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var want []oracleResult
	if err := json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(commands) {
		t.Fatalf("Python returned %d results for %d commands", len(want), len(commands))
	}
	failures := 0
	fail := func(format string, args ...any) {
		if failures++; failures <= 20 {
			t.Errorf(format, args...)
		}
	}
	for i, c := range commands {
		for j, config := range configs {
			got, w := run(config.new(c)), want[i].Configs[j]
			if !slices.Equal(got.Tokens, w.Tokens) || got.Error != w.Error || got.Lineno != w.Lineno {
				fail("%s %q = %+v; Python gives %+v", config.name, c, got, w)
			}
		}
		split, err := Split(c)
		splitError := ""
		if err != nil {
			splitError = err.Error()
		}
		if !slices.Equal(split, want[i].Split) || splitError != want[i].SplitError {
			fail("Split(%q) = %q, %q; Python gives %q, %q", c, split, splitError, want[i].Split, want[i].SplitError)
		}
		if got := Quote(c); got != want[i].Quote {
			fail("Quote(%q) = %q; Python gives %q", c, got, want[i].Quote)
		}
		if got := Join(strings.Split(c, "|")); got != want[i].Join {
			fail("Join(%q) = %q; Python gives %q", c, got, want[i].Join)
		}
	}
	if failures > 0 {
		t.Errorf("%d mismatches in %d commands", failures, len(commands))
	}
}
