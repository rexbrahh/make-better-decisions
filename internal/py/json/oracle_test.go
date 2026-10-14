package json

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	stdjson "encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// oracleScript answers one JSON request per stdin line with one JSON reply per stdout line. Text crosses
// the pipe as hex of its UTF-8 bytes with surrogatepass, so lone surrogates survive.
const oracleScript = `
import json, struct, sys
sys.path.insert(0, sys.argv[1])
from agent_decisions.transcript import loads as transcript_loads

OPTIONS = [
    dict(ensure_ascii=ascii, sort_keys=sort_keys, separators=(",", ":") if compact else None,
         indent=2 if indent else None)
    for ascii in (True, False) for sort_keys in (False, True)
    for compact in (False, True) for indent in (False, True)
]

def text(wire):
    return bytes.fromhex(wire).decode("utf-8", "surrogatepass")

def wire(s):
    return s.encode("utf-8", "surrogatepass").hex()

def outcome(call):
    try:
        return ["ok", call()]
    except (ValueError, RecursionError) as error:
        return [type(error).__name__, str(error)]

def nest(kind, n):
    value = 0
    for i in range(n):
        value = [value] if kind == "list" or kind == "mixed" and i % 2 == 0 else {"a": value}
    return value

def limit(call):
    """The deepest n for which call(n) does not raise RecursionError, and str() of the error at n + 1."""
    low, high = 1, 1 << 18
    while low < high:
        middle = (low + high + 1) // 2
        try:
            call(middle)
            low = middle
        except RecursionError:
            high = middle - 1
    try:
        call(low + 1)
    except RecursionError as error:
        return [low, str(error)]

for line in sys.stdin:
    op, *args = json.loads(line)
    if op == "loads":
        reply = outcome(lambda: [wire(json.dumps(json.loads(text(args[0])), **o)) for o in OPTIONS])
    elif op == "parse":
        reply = outcome(lambda: json.loads(text(args[0])) and None)
    elif op == "depth":
        low, high = 1, 1 << 20
        while low < high:
            middle = (low + high + 1) // 2
            try:
                json.loads("[" * middle + "]" * middle)
                low = middle
            except RecursionError:
                high = middle - 1
        reply = low
    elif op == "float":
        reply = json.dumps(struct.unpack(">d", bytes.fromhex(args[0]))[0])
    elif op == "utf8":
        reply = wire(bytes.fromhex(args[0]).decode("utf-8", "replace"))
    elif op == "loadsbytes":
        reply = outcome(lambda: wire(json.dumps(transcript_loads(bytes.fromhex(args[0])))))
    elif op == "equal":
        reply = json.loads(text(args[0])) == json.loads(text(args[1]))
    elif op == "dumpsdepth":
        reply = limit(lambda n: json.dumps(nest(args[0], n), **OPTIONS[int(args[1])]))
    elif op == "equaldepth":
        reply = limit(lambda n: nest(args[0], n) == nest(args[0], n))
    elif op == "reprgap":
        # How many list levels below a dict the encoder may overflow while repr(key) overflows too.
        lists = limit(lambda n: json.dumps(nest("list", n)))[0]
        def message(outer):
            value = {"a": nest("list", 1 << 17)}
            for _ in range(outer):
                value = [value]
            try:
                json.dumps(value)
            except RecursionError as error:
                return str(error)
        reply = [lists - outer for outer in range(lists - 12, lists + 1) if "repr" in message(outer)]
    print(json.dumps(reply))
`

// allOptions lists Options in the order of the oracle's OPTIONS.
func allOptions() []Options {
	var options []Options
	for _, allowUnicode := range []bool{false, true} {
		for _, sortKeys := range []bool{false, true} {
			for _, compact := range []bool{false, true} {
				for _, indent := range []int{0, 2} {
					options = append(options, Options{allowUnicode, sortKeys, compact, indent})
				}
			}
		}
	}
	return options
}

// askPython runs every request through one CPython process named by AGENT_DECISIONS_PYTHON and returns
// the replies in order. It skips the test when the variable is unset.
func askPython(t *testing.T, requests [][]string) []stdjson.RawMessage {
	t.Helper()
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	var input strings.Builder
	for _, request := range requests {
		line, err := stdjson.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		input.Write(line)
		input.WriteByte('\n')
	}
	command := exec.Command(python, "-I", "-c", oracleScript, repoRoot)
	command.Stdin = strings.NewReader(input.String())
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var replies []stdjson.RawMessage
	lines := bufio.NewScanner(strings.NewReader(string(output)))
	lines.Buffer(nil, 1<<26)
	for lines.Scan() {
		replies = append(replies, stdjson.RawMessage(lines.Text()))
	}
	if len(replies) != len(requests) {
		t.Fatalf("python answered %d of %d requests", len(replies), len(requests))
	}
	return replies
}

func wire(s string) string {
	return hex.EncodeToString([]byte(s))
}

// outcome is the oracle's ["ok", result] or [error class, message] reply.
func outcome(t *testing.T, reply stdjson.RawMessage) (string, stdjson.RawMessage) {
	t.Helper()
	var parts []stdjson.RawMessage
	var class string
	if err := stdjson.Unmarshal(reply, &parts); err != nil || len(parts) != 2 {
		t.Fatalf("bad oracle reply %s", reply)
	}
	if err := stdjson.Unmarshal(parts[0], &class); err != nil {
		t.Fatal(err)
	}
	return class, parts[1]
}

// errorClass returns "ok" for nil and the PyType of every error this package returns.
func errorClass(err error) string {
	if err == nil {
		return "ok"
	}
	if typed, ok := err.(interface{ PyType() string }); ok {
		return typed.PyType()
	}
	return "unexpected " + err.Error()
}

// checkLoads compares Loads and every Dumps option with the oracle's "loads" reply for text, and reports
// whether Python rejected the text.
func checkLoads(t *testing.T, text string, reply stdjson.RawMessage) bool {
	t.Helper()
	class, result := outcome(t, reply)
	value, err := Loads(text)
	if class != errorClass(err) {
		t.Errorf("Loads(%q): Go %s %v; Python %s %s", text, errorClass(err), err, class, result)
		return class != "ok"
	}
	if class != "ok" {
		var message string
		if stdjson.Unmarshal(result, &message); message != err.Error() {
			t.Errorf("Loads(%q) error: Go %q; Python %q", text, err.Error(), message)
		}
		return true
	}
	var dumped []string
	if err := stdjson.Unmarshal(result, &dumped); err != nil {
		t.Fatal(err)
	}
	for i, options := range allOptions() {
		if got := wire(dumps(t, value, options)); got != dumped[i] {
			want, _ := hex.DecodeString(dumped[i])
			t.Errorf("Dumps(Loads(%q), %+v) = %q; Python %q", text, options, dumps(t, value, options), want)
		}
	}
	return false
}

// generator writes random JSON texts that exercise the decoder's and encoder's corner cases.
type generator struct {
	rand *rand.Rand
}

func newGenerator(seed uint64) *generator {
	return &generator{rand.New(rand.NewPCG(seed, 0x5eed))}
}

func (g *generator) pick(n int) int {
	return g.rand.IntN(n)
}

func (g *generator) choose(options ...string) string {
	return options[g.pick(len(options))]
}

func (g *generator) whitespace() string {
	return g.choose("", "", "", " ", "\n", " \t", "\r\n  ")
}

func (g *generator) value(depth int) string {
	switch n := g.pick(10); {
	case n == 0 && depth < 4:
		items := make([]string, g.pick(5))
		for i := range items {
			items[i] = g.whitespace() + g.value(depth+1) + g.whitespace()
		}
		return "[" + g.whitespace() + strings.Join(items, ",") + "]"
	case n == 1 && depth < 4:
		members := make([]string, g.pick(5))
		for i := range members {
			key := g.choose(`"a"`, `"\u0061"`, `"b"`, `"é"`, `"\ud800"`, `"😀"`, `"\ud83d\ude00"`, g.str())
			members[i] = g.whitespace() + key + g.whitespace() + ":" + g.whitespace() + g.value(depth+1) + g.whitespace()
		}
		return "{" + g.whitespace() + strings.Join(members, ",") + "}"
	case n <= 3:
		return g.str()
	case n <= 5:
		return g.integer()
	case n <= 8:
		return g.float()
	}
	return g.choose("null", "true", "false", "NaN", "Infinity", "-Infinity")
}

var stringCodePoints = []rune{
	'a', 'Z', '0', ' ', '~', 0x7f, 0x00, 0x1f, '\n', '\t', '\b', '\f', '\r', '"', '\\', '/', 0xa0, 0xe9,
	0x2028, 0xfeff, 0xfffd, 0xffff, 0x1f600, 0x1d11e, 0x10ffff, 0xd800, 0xdbff, 0xdc00, 0xdfff, 0xd83d, 0xde00,
}

var shortEscapeLetters = map[rune]string{'"': `\"`, '\\': `\\`, '/': `\/`, '\b': `\b`, '\f': `\f`, '\n': `\n`, '\r': `\r`, '\t': `\t`}

// str writes a JSON string whose code points are randomly raw, short-escaped or \u-escaped.
func (g *generator) str() string {
	var b []byte
	b = append(b, '"')
	for range g.pick(8) {
		r := stringCodePoints[g.pick(len(stringCodePoints))]
		short, hasShort := shortEscapeLetters[r]
		mustEscape := r < 0x20 || r == '"' || r == '\\'
		switch choice := g.pick(3); {
		case hasShort && choice == 0:
			b = append(b, short...)
		case mustEscape || choice == 1:
			b = append(b, g.escape(r)...)
		default:
			b = appendWTF8(b, r)
		}
	}
	return string(append(b, '"'))
}

func (g *generator) escape(r rune) string {
	format := g.choose("%04x", "%04X")
	if r < 0x10000 {
		return `\u` + sprintf(format, r)
	}
	r -= 0x10000
	return `\u` + sprintf(format, 0xd800|r>>10) + `\u` + sprintf(format, 0xdc00|r&0x3ff)
}

func sprintf(format string, r rune) string {
	digits := strconv.FormatInt(int64(r), 16)
	digits = strings.Repeat("0", 4-len(digits)) + digits
	if format == "%04X" {
		return strings.ToUpper(digits)
	}
	return digits
}

func (g *generator) digits(n int) string {
	var b strings.Builder
	for range n {
		b.WriteByte(byte('0' + g.pick(10)))
	}
	return b.String()
}

func (g *generator) integer() string {
	sign := g.choose("", "", "-")
	if g.pick(4) == 0 {
		return sign + "0"
	}
	return sign + strconv.Itoa(1+g.pick(9)) + g.digits(g.pick(40))
}

var trickyFloats = []string{
	"1e16", "1e15", "1e-5", "1e-4", "5e-324", "-0.0", "1.7976931348623157e308", "1.7976931348623159e308",
	"1E400", "-1e400", "1e-400", "0.1", "2.5E+3", "4.9e-324", "2.4703282292062327e-324",
	"2.4703282292062328e-324", "2.2250738585072011e-308", "9007199254740993.0", "123456789012345678.0",
	"1.00000000000000011102230246251565404236316680908203125",
	"1.00000000000000011102230246251565404236316680908203126", "0e0", "-0E-0", "1e00000000000000000000001",
	"0.000001", "1e22", "1e23", "999999999999999999999.0",
}

func (g *generator) float() string {
	switch g.pick(3) {
	case 0:
		return trickyFloats[g.pick(len(trickyFloats))]
	case 1:
		f := math.Float64frombits(g.rand.Uint64())
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "1.5"
		}
		text := strconv.FormatFloat(f, g.choose("e", "g")[0], g.pick(26)-1, 64)
		return strings.Replace(text, "e", g.choose("e", "E"), 1)
	}
	return g.choose("", "-") + strconv.Itoa(g.pick(10)) + "." + g.digits(1+g.pick(30)) +
		g.choose("", "e", "E") + g.choose("", "+", "-") + strconv.Itoa(g.pick(400))
}

var mutationRunes = []rune(",:[]{}\"\\ \x01\n0-+.eEunltrfaNI\ufeff")

// mutate damages text with one random edit: delete, insert, replace or truncate.
func (g *generator) mutate(text string) string {
	runes := decodeWTF8(text)
	pos := g.pick(len(runes) + 1)
	r := mutationRunes[g.pick(len(mutationRunes))]
	switch g.pick(4) {
	case 0:
		if pos < len(runes) {
			runes = append(runes[:pos], runes[pos+1:]...)
		}
	case 1:
		runes = append(runes[:pos], append([]rune{r}, runes[pos:]...)...)
	case 2:
		if pos < len(runes) {
			runes[pos] = r
		}
	default:
		runes = runes[:pos]
	}
	var b []byte
	for _, r := range runes {
		b = appendWTF8(b, r)
	}
	return string(b)
}

var handMalformed = []string{
	"", " ", "[1,]", `{"a":1,}`, `{"a" 1}`, "{a:1}", "[1 2]", `"abc`, "\"a\x01\"", `"\x"`, `"\u12"`, `"\u12G4"`,
	"[1] x", "01", "1.", "1e", "-", "-I", "nan", "\ufeff[]", `"\`, `"\u`, "[", "{", `{"a"`, `{"a":`, `{"a":1`,
	"[1", "[1,", `{"a":1,`, `{"a":1 ,}`, "[1 ,]", "[1, ]", "{,}", "[,]", "1.5e", "tru", "nul", "-Infinit",
	`"\ud800\uZZZZ"`, `"\ud800\u12"`, `"\ud800\u12"x`, `"\u1234`, `"\ud800\udc00`, `"\ud800\udc00"`, "\n\n  x",
	"\"a\nb\"", "[1,\n2,\n]", `{"a":1}}`, "\u00a0[]", "[1]\x00", "[1,,2]", "[-]", "[--1]", "[+1]", "[.5]",
	"[1.e5]", "[1e+]", "[1ee5]", "[0x10]", `"\U0041"`, "1 2", `{"a":1 "b":2}`, `{"a"::1}`, `{"a":1,,}`,
	`{"a",}`, `{"a":}`, "[}", "{]", "\"\xed\xa0\x80\x01\"", "[\"😀\"\n,\n x]", strings.Repeat("1", 4301),
	"[-" + strings.Repeat("1", 4301) + ",", "0" + strings.Repeat("1", 5000), strings.Repeat("1", 5000) + ".0",
	"[\"\\ud83d\\ude00\" 1]", "{\"\\ud83d\" 1}", "\"\t\"", "\"\x1f\"", "\"\x7f\"", "[NaN, -NaN]", "Infinityx",
}

func TestLoadsDumpsMatchPython(t *testing.T) {
	g := newGenerator(1)
	var texts []string
	for range 3000 {
		texts = append(texts, g.whitespace()+g.value(0)+g.whitespace())
	}
	for i := range 4000 {
		texts = append(texts, g.mutate(texts[i%3000]))
	}
	texts = append(texts, handMalformed...)
	requests := make([][]string, len(texts))
	for i, text := range texts {
		requests[i] = []string{"loads", wire(text)}
	}
	replies := askPython(t, requests)
	malformed := 0
	for i, text := range texts {
		if checkLoads(t, text, replies[i]) {
			malformed++
		}
	}
	if malformed < 200 {
		t.Errorf("only %d malformed inputs; want at least 200", malformed)
	}
	t.Logf("%d texts, %d malformed, %d Dumps option sets each", len(texts), malformed, len(allOptions()))
}

// TestRecursionLimitMatchesPython checks that maxDepth is within depthTolerance of the oracle's own
// limit, and that both sides agree exactly on documents that nest depthTolerance below or above it. The
// limit depends on the oracle's argv and environment, so the boundary itself cannot match exactly.
func TestRecursionLimitMatchesPython(t *testing.T) {
	const depthTolerance = 1000
	// Alternating "[{" nesting is only checked below the limit: past it, the container kind that overflows
	// depends on the parity of the oracle's limit.
	below := maxDepth - depthTolerance
	texts := []string{strings.Repeat(`[{"a":`, below/2) + strings.Repeat("}]", below/2)}
	for _, depth := range []int{below, maxDepth + depthTolerance} {
		texts = append(texts,
			strings.Repeat("[", depth)+strings.Repeat("]", depth),
			strings.Repeat(`{"a":`, depth)+"0"+strings.Repeat("}", depth),
			strings.Repeat("[", depth)+"x")
	}
	requests := [][]string{{"depth"}}
	for _, text := range texts {
		requests = append(requests, []string{"parse", wire(text)})
	}
	replies := askPython(t, requests)
	pythonDepth, err := strconv.Atoi(string(replies[0]))
	if err != nil || pythonDepth < maxDepth-depthTolerance || pythonDepth > maxDepth+depthTolerance {
		t.Errorf("Python decodes %s levels; maxDepth is %d", replies[0], maxDepth)
	}
	t.Logf("Python limit in the oracle process: %d levels; maxDepth: %d", pythonDepth, maxDepth)
	for i, text := range texts {
		class, result := outcome(t, replies[i+1])
		_, err := Loads(text)
		var message string
		stdjson.Unmarshal(result, &message)
		if class != errorClass(err) || class != "ok" && message != err.Error() {
			t.Errorf("case %d: Go %s %v; Python %s %s", i, errorClass(err), err, class, result)
		}
	}
}

func TestDumpsFloatMatchesPython(t *testing.T) {
	g := newGenerator(2)
	bits := []uint64{0, 1 << 63, 1, math.Float64bits(math.NaN()), math.Float64bits(math.Inf(-1)), 0x7fefffffffffffff}
	for range 10000 {
		bits = append(bits, g.rand.Uint64())
	}
	for _, f := range []float64{1e16, 1e15, 1e-5, 1e-4, 9999999999999998, 0.0001000000000000001, 1e22, 1e21} {
		bits = append(bits, math.Float64bits(f), math.Float64bits(-f))
	}
	requests := make([][]string, len(bits))
	for i, b := range bits {
		requests[i] = []string{"float", hex.EncodeToString(binary.BigEndian.AppendUint64(nil, b))}
	}
	for i, reply := range askPython(t, requests) {
		var want string
		stdjson.Unmarshal(reply, &want)
		if got := dumps(t, math.Float64frombits(bits[i]), Options{}); got != want {
			t.Errorf("Dumps(%#016x) = %q; Python %q", bits[i], got, want)
		}
	}
}

var byteChunks = []string{
	"a", " ", `"`, `\`, `\n`, `\ud83d`, `\uDE00`, `\udc00`, `\uD800\uDC00`, `\u00e9`, `\uZZ`, "\x00", "\x7f",
	"\x80", "\xbf", "\xc0", "\xc1", "\xc2", "\xdf", "\xe0", "\xe0\xa0", "\xe0\x9f", "\xe1\x80", "\xed",
	"\xed\x9f", "\xed\xa0", "\xed\xa0\x80", "\xed\xbf\xbf", "\xef\xbf\xbd", "\xf0", "\xf0\x90", "\xf0\x8f",
	"\xf0\x90\x80", "\xf4\x8f\xbf", "\xf4\x90", "\xf5", "\xff", "é", "😀", "\xef\xbb\xbf",
}

func TestBytesMatchPython(t *testing.T) {
	g := newGenerator(3)
	var datas, documents []string
	for range 10000 {
		var data strings.Builder
		for range g.pick(9) {
			if g.pick(4) == 0 {
				data.WriteByte(byte(g.rand.UintN(256)))
			} else {
				data.WriteString(byteChunks[g.pick(len(byteChunks))])
			}
		}
		datas = append(datas, data.String())
		wrapper := g.choose(`"%s"`, `["%s", 1]`, `{"%s": "%s"}`, "%s")
		documents = append(documents, strings.ReplaceAll(wrapper, "%s", data.String()))
	}
	var requests [][]string
	for i := range datas {
		requests = append(requests, []string{"utf8", wire(datas[i])}, []string{"loadsbytes", wire(documents[i])})
	}
	replies := askPython(t, requests)
	parsed := 0
	for i := range datas {
		var decoded string
		stdjson.Unmarshal(replies[2*i], &decoded)
		if got := wire(DecodeUTF8([]byte(datas[i]))); got != decoded {
			t.Errorf("DecodeUTF8(%q) = %q; Python %s", datas[i], got, decoded)
		}
		class, result := outcome(t, replies[2*i+1])
		value, err := LoadsBytes([]byte(documents[i]), 4)
		var want string
		stdjson.Unmarshal(result, &want)
		switch {
		case class != errorClass(err):
			t.Errorf("LoadsBytes(%q): Go %s %v; Python %s %s", documents[i], errorClass(err), err, class, result)
		case class != "ok" && want != err.Error():
			t.Errorf("LoadsBytes(%q) error: Go %q; Python %q", documents[i], err.Error(), want)
		case class == "ok" && wire(dumps(t, value, Options{})) != want:
			t.Errorf("LoadsBytes(%q) = %s; Python %s", documents[i], dumps(t, value, Options{}), want)
		case class == "ok":
			parsed++
		}
	}
	t.Logf("%d byte strings, %d documents parsed", len(datas), parsed)
}

func TestEqualMatchesPython(t *testing.T) {
	pool := []string{
		"1", "1.0", "true", "false", "0", "-0.0", "0.0", "null", `""`, `"a"`, "[]", "{}", "[1]", "[1.0]", "[true]",
		`["a"]`, `{"a":1,"b":2}`, `{"b":2,"a":1}`, `{"a":1.0,"b":true}`, `{"a":1}`, "9007199254740993",
		"9007199254740992", "9007199254740992.0", "1e400", "Infinity", "-Infinity", "100000000000000000000000000000000",
		"1e32", `[[1,2],{"x":null}]`, `[[1,2.0],{"x":null}]`, `[[1,2],{"x":false}]`, "2", "0.5",
	}
	var requests [][]string
	for _, a := range pool {
		for _, b := range pool {
			requests = append(requests, []string{"equal", wire(a), wire(b)})
		}
	}
	replies := askPython(t, requests)
	for i, request := range requests {
		a, b := pool[i/len(pool)], pool[i%len(pool)]
		valueA, errA := Loads(a)
		valueB, errB := Loads(b)
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if got := strconv.FormatBool(equals(t, valueA, valueB)); got != string(replies[i]) {
			t.Errorf("Equal(%s, %s) = %s; Python %s (request %v)", a, b, got, replies[i], request)
		}
	}
}

// TestDumpsEqualLimitsMatchPython compares the nesting at which Dumps and Equal raise with the oracle's,
// for every Options set that agent-decisions passes. The C stack limit depends on the oracle's argv and
// environment, so the depth only has to be within the tolerance of TestRecursionLimitMatchesPython; the
// message must match exactly.
func TestDumpsEqualLimitsMatchPython(t *testing.T) {
	const depthTolerance = 1000
	type probe struct {
		request []string
		call    func(n int) error
	}
	var probes []probe
	options := allOptions()
	// sort_keys + compact, sort_keys, ensure_ascii=False, sort_keys + ensure_ascii=False. indent=2 has the
	// same limits (measured once), but its output grows with the square of the depth: 20 GB at the limit.
	for _, index := range []int{6, 4, 8, 12} {
		for _, kind := range []string{"list", "dict"} {
			probes = append(probes, probe{[]string{"dumpsdepth", kind, strconv.Itoa(index)}, func(n int) error {
				_, err := Dumps(nest(kind, n), options[index])
				return err
			}})
		}
	}
	probes = append(probes, probe{[]string{"dumpsdepth", "mixed", "0"}, func(n int) error {
		_, err := Dumps(nest("mixed", n), Options{})
		return err
	}})
	for _, kind := range []string{"list", "dict", "mixed"} {
		probes = append(probes, probe{[]string{"equaldepth", kind}, func(n int) error {
			_, err := Equal(nest(kind, n), nest(kind, n))
			return err
		}})
	}
	requests := make([][]string, len(probes))
	for i, p := range probes {
		requests[i] = p.request
	}
	for i, reply := range askPython(t, requests) {
		var want struct {
			Depth int
			Msg   string
		}
		var pair []stdjson.RawMessage
		if stdjson.Unmarshal(reply, &pair) != nil || len(pair) != 2 ||
			stdjson.Unmarshal(pair[0], &want.Depth) != nil || stdjson.Unmarshal(pair[1], &want.Msg) != nil {
			t.Fatalf("bad oracle reply %s", reply)
		}
		depth, err := limit(probes[i].call)
		if depth < want.Depth-depthTolerance || depth > want.Depth+depthTolerance || err == nil ||
			err.Error() != want.Msg || errorClass(err) != "RecursionError" {
			t.Errorf("%v: Go %d then %v; Python %d then %q", probes[i].request, depth, err, want.Depth, want.Msg)
		}
		t.Logf("%v: Go %d, Python %d levels", probes[i].request, depth, want.Depth)
	}
}

// TestReprNoteGapMatchesPython checks that the dicts whose repr(key) note overflows sit the same number
// of list levels above the overflow in Go and in the oracle, which does not depend on the stack's size.
func TestReprNoteGapMatchesPython(t *testing.T) {
	replies := askPython(t, [][]string{{"reprgap"}})
	lists, _ := limit(func(n int) error { _, err := Dumps(nest("list", n), Options{}); return err })
	var gaps []int
	for outer := lists - 12; outer <= lists; outer++ {
		var value any = NewObject("a", nest("list", 1<<17))
		for range outer {
			value = []any{value}
		}
		if _, err := Dumps(value, Options{}); err != nil && strings.Contains(err.Error(), "repr") {
			gaps = append(gaps, lists-outer)
		}
	}
	var want []int
	if err := stdjson.Unmarshal(replies[0], &want); err != nil || !slices.Equal(gaps, want) {
		t.Errorf("dict note gaps: Go %v; Python %s", gaps, replies[0])
	}
}

// TestLoadsBytesLimitMatchesPython checks the surrogate pass's recursion limit exactly: it counts Python
// frames, not stack bytes. The oracle calls loads 4 frames deep (<module>, outcome, a lambda, loads).
func TestLoadsBytesLimitMatchesPython(t *testing.T) {
	var documents []string
	for _, depth := range []int{994, 995, 996} {
		documents = append(documents,
			strings.Repeat("[", depth)+`"\ud800"`+strings.Repeat("]", depth),
			strings.Repeat(`{"a":`, depth)+`"\udfff"`+strings.Repeat("}", depth),
			`["\ud800",`+strings.Repeat("[", depth)+"0"+strings.Repeat("]", depth)+"]",
			`{"\ud800":`+strings.Repeat("[", depth)+"{}"+strings.Repeat("]", depth)+"}")
	}
	requests := make([][]string, len(documents))
	for i, document := range documents {
		requests[i] = []string{"loadsbytes", wire(document)}
	}
	raised := 0
	for i, reply := range askPython(t, requests) {
		class, result := outcome(t, reply)
		var message string
		stdjson.Unmarshal(result, &message)
		_, err := LoadsBytes([]byte(documents[i]), 4)
		if class != errorClass(err) || class != "ok" && message != err.Error() {
			t.Errorf("document %d: Go %s %v; Python %s %s", i, errorClass(err), err, class, result)
		}
		if class == "RecursionError" {
			raised++
		}
	}
	if raised == 0 || raised == len(documents) {
		t.Errorf("%d of %d documents raised; the depths miss the limit", raised, len(documents))
	}
}

// TestDecodeUTF8StrictMatchesPython compares DecodeUTF8Strict with bytes.decode("utf-8") on byte strings
// built from byteChunks. It runs its own interpreter: a longer oracleScript moves the C stack limit that
// TestReprNoteGapMatchesPython measures.
func TestDecodeUTF8StrictMatchesPython(t *testing.T) {
	python := os.Getenv("AGENT_DECISIONS_PYTHON")
	if python == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON to a CPython 3.15 interpreter to compare with the oracle")
	}
	g := newGenerator(5)
	datas := make([]string, 10000)
	for i := range datas {
		var data strings.Builder
		for range g.pick(9) {
			if g.pick(4) == 0 {
				data.WriteByte(byte(g.rand.UintN(256)))
			} else {
				data.WriteString(byteChunks[g.pick(len(byteChunks))])
			}
		}
		datas[i] = wire(data.String())
	}
	input, err := stdjson.Marshal(datas)
	if err != nil {
		t.Fatal(err)
	}
	script := `import json, sys
out = []
for data in json.load(sys.stdin):
    try:
        out.append(["ok", bytes.fromhex(data).decode("utf-8").encode("utf-8", "surrogatepass").hex()])
    except UnicodeDecodeError as error:
        out.append(["UnicodeDecodeError", str(error)])
print(json.dumps(out))`
	command := exec.Command(python, "-I", "-c", script)
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var want [][2]string
	if err := stdjson.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	for i, data := range datas {
		raw, _ := hex.DecodeString(data)
		text, err := DecodeUTF8Strict(raw)
		got := [2]string{"ok", wire(text)}
		if err != nil {
			got = [2]string{errorClass(err), err.Error()}
		}
		if got != want[i] {
			t.Errorf("DecodeUTF8Strict(%q) = %q; Python %q", raw, got, want[i])
		}
	}
	t.Logf("%d byte strings", len(datas))
}
