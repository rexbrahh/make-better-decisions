package json

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestDumpsFloat(t *testing.T) {
	cases := []struct {
		f    float64
		want string
	}{
		{1e16, "1e+16"},
		{1e15, "1000000000000000.0"},
		{1e-5, "1e-05"},
		{1e-4, "0.0001"},
		{5e-324, "5e-324"},
		{math.Copysign(0, -1), "-0.0"},
		{0, "0.0"},
		{1.7976931348623157e308, "1.7976931348623157e+308"},
		{0.1, "0.1"},
		{123456789012345678, "1.2345678901234568e+17"},
		{1.5, "1.5"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
	}
	for _, c := range cases {
		if got := dumps(t, c.f, Options{}); got != c.want {
			t.Errorf("Dumps(%v) = %q; want %q", c.f, got, c.want)
		}
	}
}

func TestDumpsOptions(t *testing.T) {
	value := NewObject("b", []any{IntOf(1), 2, int64(-3)}, "a", NewObject(), "é", []string{"x"}, "c", []any{})
	cases := []struct {
		options Options
		want    string
	}{
		{Options{}, `{"b": [1, 2, -3], "a": {}, "\u00e9": ["x"], "c": []}`},
		{Options{AllowUnicode: true, SortKeys: true, Compact: true}, `{"a":{},"b":[1,2,-3],"c":[],"é":["x"]}`},
		{Options{Indent: 2}, "{\n  \"b\": [\n    1,\n    2,\n    -3\n  ],\n  \"a\": {},\n  \"\\u00e9\": [\n    \"x\"\n  ],\n  \"c\": []\n}"},
		{Options{Indent: 1, Compact: true}, "{\n \"b\":[\n  1,\n  2,\n  -3\n ],\n \"a\":{},\n \"\\u00e9\":[\n  \"x\"\n ],\n \"c\":[]\n}"},
	}
	for _, c := range cases {
		if got := dumps(t, value, c.options); got != c.want {
			t.Errorf("Dumps(%+v) = %q; want %q", c.options, got, c.want)
		}
	}
}

func TestDumpsString(t *testing.T) {
	cases := []struct{ s, ascii, unicode string }{
		{"a\"\\/\b\f\n\r\t\x00\x1f\x7f", `"a\"\\/\b\f\n\r\t\u0000\u001f\u007f"`, "\"a\\\"\\\\/\\b\\f\\n\\r\\t\\u0000\\u001f\x7f\""},
		{"é\u2028😀", `"\u00e9\u2028\ud83d\ude00"`, "\"é\u2028😀\""},
		{"\xed\xa0\x80x\xed\xbf\xbf", `"\ud800x\udfff"`, "\"\xed\xa0\x80x\xed\xbf\xbf\""},
	}
	for _, c := range cases {
		if got := dumps(t, c.s, Options{}); got != c.ascii {
			t.Errorf("Dumps(%q) = %q; want %q", c.s, got, c.ascii)
		}
		if got := dumps(t, c.s, Options{AllowUnicode: true}); got != c.unicode {
			t.Errorf("Dumps(%q, ensure_ascii=False) = %q; want %q", c.s, got, c.unicode)
		}
	}
}

func TestLoads(t *testing.T) {
	cases := []struct {
		text string
		want any
	}{
		{"-0", Int("0")},
		{" 12345678901234567890123 ", Int("12345678901234567890123")},
		{"-0.0", math.Copysign(0, -1)},
		{"1E400", math.Inf(1)},
		{"1e-400", 0.0},
		{`"\ud83d\ude00"`, "😀"},
		{`"\ud800"`, "\xed\xa0\x80"},
		{`"\udc00\ud800"`, "\xed\xb0\x80\xed\xa0\x80"},
		{"\"\xed\xa0\x80\"", "\xed\xa0\x80"},
		{`["\u00E9", null, true, false]`, []any{"é", nil, true, false}},
		{`{"a": 1, "b": 2, "a": 3}`, NewObject("a", IntOf(3), "b", IntOf(2))},
	}
	for _, c := range cases {
		got, err := Loads(c.text)
		if err != nil || !equals(t, got, c.want) || dumps(t, got, Options{}) != dumps(t, c.want, Options{}) {
			t.Errorf("Loads(%q) = %#v, %v; want %#v", c.text, got, err, c.want)
		}
	}
	if got, _ := Loads(`{"a": 1, "b": 2, "a": 3}`); !slices.Equal(got.(*Object).Keys(), []string{"a", "b"}) {
		t.Errorf("a duplicate key moved: %v", got.(*Object).Keys())
	}
	if got, _ := Loads("NaN"); !math.IsNaN(got.(float64)) {
		t.Errorf("Loads(NaN) = %v", got)
	}
}

func TestLoadsErrors(t *testing.T) {
	cases := []struct{ text, want string }{
		{"", "Expecting value: line 1 column 1 (char 0)"},
		{"[1,]", "Illegal trailing comma before end of array: line 1 column 3 (char 2)"},
		{`{"a":1 ,}`, "Illegal trailing comma before end of object: line 1 column 8 (char 7)"},
		{`{"a" 1}`, "Expecting ':' delimiter: line 1 column 6 (char 5)"},
		{"{a:1}", "Expecting property name enclosed in double quotes: line 1 column 2 (char 1)"},
		{"[1 2]", "Expecting ',' delimiter: line 1 column 4 (char 3)"},
		{`"abc`, "Unterminated string starting at: line 1 column 1 (char 0)"},
		{"\"a\x01\"", "Invalid control character at: line 1 column 3 (char 2)"},
		{`"\x"`, "Invalid \\escape: line 1 column 2 (char 1)"},
		{`"\u12G4"`, "Invalid \\uXXXX escape: line 1 column 3 (char 2)"},
		{`"\ud800\uZZZZ"`, "Invalid \\uXXXX escape: line 1 column 9 (char 8)"},
		{`"\u1234`, "Unterminated string starting at: line 1 column 1 (char 0)"},
		{`"\u123`, "Invalid \\uXXXX escape: line 1 column 3 (char 2)"},
		{"[1] x", "Extra data: line 1 column 5 (char 4)"},
		{"1.", "Extra data: line 1 column 2 (char 1)"},
		{"\ufeff[]", "Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)"},
		{"[1,\n2,\n]", "Illegal trailing comma before end of array: line 2 column 2 (char 5)"},
		{"[\"é😀\", x]", "Expecting value: line 1 column 8 (char 7)"},
	}
	for _, c := range cases {
		_, err := Loads(c.text)
		var decodeError *DecodeError
		if !errors.As(err, &decodeError) || err.Error() != c.want {
			t.Errorf("Loads(%q) error = %v; want %q", c.text, err, c.want)
		}
	}
}

func TestLoadsLimits(t *testing.T) {
	if _, err := Loads(strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth)); err != nil {
		t.Errorf("depth %d: %v", maxDepth, err)
	}
	deeper := strings.Repeat(`{"a":`, maxDepth+1) + "0" + strings.Repeat("}", maxDepth+1)
	if _, err := Loads(deeper); !errors.As(err, new(*RecursionError)) || !strings.HasSuffix(err.Error(), "JSON object from a unicode string") {
		t.Errorf("depth %d: %v; want a RecursionError", maxDepth+1, err)
	}
	if _, err := Loads(strings.Repeat("1", 4300)); err != nil {
		t.Errorf("4300 digits: %v", err)
	}
	if _, err := Loads("[-" + strings.Repeat("1", 4301) + ","); !errors.As(err, new(*IntDigitsError)) || !strings.Contains(err.Error(), "value has 4301 digits") {
		t.Errorf("4301 digits: %v; want an IntDigitsError", err)
	}
}

func TestDecodeUTF8(t *testing.T) {
	cases := []struct{ data, want string }{
		{"ok é 😀", "ok é 😀"},
		{"\xe2\x82", "\ufffd"},
		{"\xe2\x82x", "\ufffdx"},
		{"\xed\xa0\x80", "\ufffd\ufffd\ufffd"},
		{"\xf0\x90\x80", "\ufffd"},
		{"\xc0\xaf", "\ufffd\ufffd"},
		{"\xf4\x90\x80\x80", "\ufffd\ufffd\ufffd\ufffd"},
		{"\xe0\x80\xaf", "\ufffd\ufffd\ufffd"},
		{"\xef\xbf\xbd", "\ufffd"},
	}
	for _, c := range cases {
		if got := DecodeUTF8([]byte(c.data)); got != c.want {
			t.Errorf("DecodeUTF8(%q) = %q; want %q", c.data, got, c.want)
		}
	}
}

func TestLoadsBytes(t *testing.T) {
	cases := []struct{ data, want string }{
		{`["\ud800", "\ud83d\ude00", "` + "\xff" + `"]`, `["\ufffd", "\ud83d\ude00", "\ufffd"]`},
		{`{"\ud800": 1, "\udc00": 2}`, `{"\ufffd": 2}`},
		{"\"\xed\xa0\x80\"", `"\ufffd\ufffd\ufffd"`},
		{`"\u00e9"`, `"\u00e9"`},
	}
	for _, c := range cases {
		got, err := LoadsBytes([]byte(c.data), 4)
		if err != nil || dumps(t, got, Options{}) != c.want {
			t.Errorf("LoadsBytes(%q) = %v, %v; want %s", c.data, got, err, c.want)
		}
	}
}

func TestObject(t *testing.T) {
	object := NewObject("a", 1, "b", 2, "c", 3)
	object.Set("a", 4)
	if !object.Delete("b") || object.Delete("b") {
		t.Error("Delete did not report presence")
	}
	object.Set("b", 5)
	copied := object.Copy()
	copied.Set("d", 6)
	if !slices.Equal(object.Keys(), []string{"a", "c", "b"}) || object.Len() != 3 || copied.Len() != 4 {
		t.Errorf("keys %v, copy keys %v", object.Keys(), copied.Keys())
	}
	if value, ok := object.Get("a"); !ok || value != 4 || object.Value("missing") != nil {
		t.Errorf("Get(a) = %v, %v", value, ok)
	}
	var zero Object
	zero.Set("x", nil)
	if zero.Len() != 1 {
		t.Error("the zero Object is not usable")
	}
}

func TestIntOf(t *testing.T) {
	if n, ok := IntOf(math.MinInt64).Int64(); !ok || n != math.MinInt64 {
		t.Errorf("Int64 round trip = %d, %v", n, ok)
	}
	if _, ok := Int("9223372036854775808").Int64(); ok {
		t.Error("Int64 accepted an overflow")
	}
}

func TestTruthy(t *testing.T) {
	falsy := []any{nil, false, Int("0"), 0, int64(0), 0.0, math.Copysign(0, -1), "", []any{}, []string{}, NewObject()}
	truthy := []any{true, Int("-1"), 1, int64(2), math.NaN(), "0", []any{nil}, []string{""}, NewObject("", nil)}
	for _, v := range falsy {
		if Truthy(v) {
			t.Errorf("Truthy(%#v) = true", v)
		}
	}
	for _, v := range truthy {
		if !Truthy(v) {
			t.Errorf("Truthy(%#v) = false", v)
		}
	}
}

func TestEqual(t *testing.T) {
	equal := [][2]any{
		{true, 1}, {Int("1"), 1.0}, {int64(0), -0.0}, {false, Int("0")}, {nil, nil},
		{Int("9007199254740992"), 9007199254740992.0}, {[]string{"a"}, []any{"a"}},
		{NewObject("a", 1, "b", []any{2}), NewObject("b", []any{2.0}, "a", true)},
	}
	unequal := [][2]any{
		{math.NaN(), math.NaN()}, {Int("9007199254740993"), 9007199254740992.0}, {1, "1"}, {nil, false},
		{[]any{1}, []any{1, 1}}, {NewObject("a", 1), NewObject("b", 1)}, {[]any{}, NewObject()},
		{Int("1"), math.Inf(1)}, {0.5, Int("0")},
	}
	for _, c := range equal {
		if !equals(t, c[0], c[1]) || !equals(t, c[1], c[0]) {
			t.Errorf("Equal(%#v, %#v) = false", c[0], c[1])
		}
	}
	for _, c := range unequal {
		if equals(t, c[0], c[1]) || equals(t, c[1], c[0]) {
			t.Errorf("Equal(%#v, %#v) = true", c[0], c[1])
		}
	}
}

func TestUnsupportedTypePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Dumps accepted a map")
		}
	}()
	_, _ = Dumps(map[string]any{}, Options{})
}

// dumps returns Dumps(v, o) and fails the test on an error.
func dumps(t *testing.T, v any, o Options) string {
	t.Helper()
	text, err := Dumps(v, o)
	if err != nil {
		t.Fatalf("Dumps: %v", err)
	}
	return text
}

// equals returns Equal(a, b) and fails the test on an error.
func equals(t *testing.T, a, b any) bool {
	t.Helper()
	same, err := Equal(a, b)
	if err != nil {
		t.Fatalf("Equal: %v", err)
	}
	return same
}

// nest returns 0 inside n containers: lists, dicts {"a": ...}, or ("mixed") lists at even levels counted
// from the inside, as the oracle's nest builds them.
func nest(kind string, n int) any {
	var value any = IntOf(0)
	for i := range n {
		if kind == "list" || kind == "mixed" && i%2 == 0 {
			value = []any{value}
		} else {
			value = NewObject("a", value)
		}
	}
	return value
}

// limit returns the deepest n for which call(n) returns no error, and the error at n + 1.
func limit(call func(n int) error) (int, error) {
	low, high := 1, 1<<18
	for low < high {
		middle := (low + high + 1) / 2
		if call(middle) == nil {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low, call(low + 1)
}

// TestRecursionLimits pins the limits measured with CPython 3.15; see encodeStack, compareStack and
// LoadsBytes.
func TestRecursionLimits(t *testing.T) {
	dumpsAt := func(kind string, o Options) func(int) error {
		return func(n int) error { _, err := Dumps(nest(kind, n), o); return err }
	}
	equalAt := func(kind string) func(int) error {
		return func(n int) error { _, err := Equal(nest(kind, n), nest(kind, n)); return err }
	}
	loadsAt := func(open, leaf, close string) func(int) error {
		return func(n int) error {
			_, err := LoadsBytes([]byte(strings.Repeat(open, n)+leaf+strings.Repeat(close, n)), 4)
			return err
		}
	}
	const encoding = "Stack overflow (used 16352 kB) while encoding a JSON object"
	const repr = "Stack overflow (used 16352 kB) while getting the repr of an object"
	const comparison = "Stack overflow (used 16352 kB) in comparison"
	const recursion = "maximum recursion depth exceeded"
	cases := []struct {
		name  string
		call  func(int) error
		depth int
		msg   string
	}{
		{"dumps list", dumpsAt("list", Options{SortKeys: true, Compact: true}), 104557, encoding},
		{"dumps dict", dumpsAt("dict", Options{SortKeys: true}), 61504, repr},
		{"dumps mixed", dumpsAt("mixed", Options{AllowUnicode: true}), 77449, repr},
		{"equal list", equalAt("list"), 47527, comparison},
		{"equal dict", equalAt("dict"), 45461, comparison},
		{"equal mixed", equalAt("mixed"), 46471, comparison},
		{"loadsbytes list", loadsAt("[", `"\ud800"`, "]"), 995, recursion},
		{"loadsbytes dict", loadsAt(`{"a":`, `"\ud800"`, "}"), 995, recursion},
		{"loadsbytes int leaf", loadsAt("[", "0", "]"), 0, ""}, // no surrogate escape: no second pass
	}
	for _, c := range cases {
		if c.depth == 0 {
			if err := c.call(5000); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err := c.call(c.depth); err != nil {
			t.Errorf("%s at %d: %v", c.name, c.depth, err)
		}
		err := c.call(c.depth + 1)
		if errorClass(err) != "RecursionError" || err.Error() != c.msg {
			t.Errorf("%s at %d: %v; want RecursionError %q", c.name, c.depth+1, err, c.msg)
		}
	}
	// A dict 1-4 list levels above the overflow cannot format its note; one 5 levels up can.
	for outer, want := range map[int]string{104552: encoding, 104553: repr, 104556: repr, 104557: encoding} {
		var value any = NewObject("a", nest("list", 1<<17))
		for range outer {
			value = []any{value}
		}
		if _, err := Dumps(value, Options{}); err == nil || err.Error() != want {
			t.Errorf("dict under %d lists: %v; want %q", outer, err, want)
		}
	}
}
