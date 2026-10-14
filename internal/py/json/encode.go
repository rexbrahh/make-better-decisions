package json

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Options selects the json.dumps arguments that agent-decisions uses. The zero value is json.dumps(v).
type Options struct {
	AllowUnicode bool // ensure_ascii=False
	SortKeys     bool // sort_keys=True
	Compact      bool // separators=(",", ":")
	Indent       int  // indent=N when above 0; 0 means indent=None
}

// The C encoder checks its stack before it encodes each list or dict, and fails when the enclosing
// containers already use more than encodeStack bytes. Measured with CPython 3.15 on darwin (16 MB main
// stack, `python3 script.py`): 104557 nested lists or 61504 nested dicts encode and one more level raises,
// for sort_keys with and without compact separators, ensure_ascii=False with and without sort_keys, and
// indent=2, so a list level takes 160 bytes and a dict level 272. Alternating list/dict nesting (77449
// levels) and one dict per three levels (84776) fit the same sums; together the four measurements put
// encodeStack in [16728960, 16729040). As with maxDepth, argv and the environment share that stack and move
// the limit by a few levels per kilobyte.
//
// The RecursionError then gains a note per enclosing container. A dict's note holds repr(key), whose own
// stack check fails while the dict's enclosing containers use more than encodeStack-encodeReprStack bytes;
// the exception then says "while getting the repr of an object" (measured: a dict under 104553 lists
// fails so, under 104552 it does not, which puts encodeReprStack in (520, 680]).
//
// py: Modules/_json.c encoder_listencode_obj, encoder_encode_key_value (Python 3.15)
const (
	encodeStack     = 16_729_000
	encodeListFrame = 160
	encodeDictFrame = 272
	encodeReprStack = 600
)

// Dumps returns json.dumps(v) with the arguments that o selects, byte for byte, or the *RecursionError
// that CPython raises for too deep nesting. It panics on a Go type outside the value model, which is a
// programming error.
//
// py: Lib/json/encoder.py JSONEncoder.encode, Modules/_json.c encoder_listencode_obj (Python 3.15)
func Dumps(v any, o Options) (string, error) {
	e := &encoder{ascii: !o.AllowUnicode, sortKeys: o.SortKeys, itemSeparator: ", ", keySeparator: ": "}
	if o.Indent > 0 {
		e.indent = strings.Repeat(" ", o.Indent)
		e.itemSeparator = ","
	}
	if o.Compact {
		e.itemSeparator, e.keySeparator = ",", ":"
	}
	if err := e.encode(v, 0); err != nil {
		return "", err
	}
	return string(e.out), nil
}

type encoder struct {
	out           []byte
	ascii         bool
	sortKeys      bool
	indent        string // "" means indent=None
	itemSeparator string
	keySeparator  string
	stack         int // bytes of C stack that the enclosing containers use
}

// py: Lib/json/encoder.py _make_iterencode._iterencode (Python 3.15)
func (e *encoder) encode(v any, level int) error {
	switch v := v.(type) {
	case nil:
		e.out = append(e.out, "null"...)
	case bool:
		e.out = strconv.AppendBool(e.out, v)
	case Int:
		e.out = append(e.out, v...)
	case int:
		e.out = strconv.AppendInt(e.out, int64(v), 10)
	case int64:
		e.out = strconv.AppendInt(e.out, v, 10)
	case float64:
		e.out = append(e.out, floatRepr(v)...)
	case string:
		e.out = appendQuoted(e.out, v, e.ascii)
	case []any, []string:
		return e.nested(encodeListFrame, func() error { return e.encodeList(asList(v), level) })
	case *Object:
		return e.nested(encodeDictFrame, func() error { return e.encodeObject(v, level) })
	default:
		panic(fmt.Sprintf("json.Dumps: unsupported type %T", v))
	}
	return nil
}

// nested runs encodeContainer under the stack check, with frame more bytes in use.
//
// py: Modules/_json.c encoder_listencode_obj _Py_EnterRecursiveCall (Python 3.15)
func (e *encoder) nested(frame int, encodeContainer func() error) error {
	if e.stack > encodeStack {
		return stackOverflow(" while encoding a JSON object")
	}
	e.stack += frame
	err := encodeContainer()
	e.stack -= frame
	return err
}

// newline starts a line indented to level. It writes nothing when indent is None.
func (e *encoder) newline(level int) {
	if e.indent != "" {
		e.out = append(e.out, '\n')
		e.out = append(e.out, strings.Repeat(e.indent, level)...)
	}
}

// py: Lib/json/encoder.py _make_iterencode._iterencode_list (Python 3.15)
func (e *encoder) encodeList(list []any, level int) error {
	if len(list) == 0 {
		e.out = append(e.out, "[]"...)
		return nil
	}
	e.out = append(e.out, '[')
	for i, item := range list {
		if i > 0 {
			e.out = append(e.out, e.itemSeparator...)
		}
		e.newline(level + 1)
		if err := e.encode(item, level+1); err != nil {
			return err
		}
	}
	e.newline(level)
	e.out = append(e.out, ']')
	return nil
}

// py: Lib/json/encoder.py _make_iterencode._iterencode_dict (Python 3.15)
func (e *encoder) encodeObject(object *Object, level int) error {
	if object.Len() == 0 {
		e.out = append(e.out, "{}"...)
		return nil
	}
	keys := object.Keys()
	if e.sortKeys {
		slices.Sort(keys) // byte order of WTF-8 is code point order, as Python sorts str
	}
	e.out = append(e.out, '{')
	for i, key := range keys {
		if i > 0 {
			e.out = append(e.out, e.itemSeparator...)
		}
		e.newline(level + 1)
		e.out = appendQuoted(e.out, key, e.ascii)
		e.out = append(e.out, e.keySeparator...)
		if err := e.encode(object.values[key], level+1); err != nil {
			if e.stack-encodeDictFrame > encodeStack-encodeReprStack { // the note's repr(key) overflows too
				err = stackOverflow(" while getting the repr of an object")
			}
			return err
		}
	}
	e.newline(level)
	e.out = append(e.out, '}')
	return nil
}

// floatRepr returns float.__repr__(f), or the JSON words for NaN and the infinities. repr uses the
// shortest round-trip digits, in fixed notation when the decimal exponent is in [-4, 16) and in
// exponent notation otherwise.
//
// py: Lib/json/encoder.py floatstr, Python/pystrtod.c format_float_short (Python 3.15)
func floatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	scientific := strconv.FormatFloat(f, 'e', -1, 64) // like "1.5e+16" or "5e-324"
	_, exponentText, _ := strings.Cut(scientific, "e")
	exponent, _ := strconv.Atoi(exponentText)
	if exponent < -4 || exponent >= 16 {
		return scientific
	}
	fixed := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(fixed, ".") {
		fixed += ".0"
	}
	return fixed
}

// shortEscapes are the two-character escapes the encoder prefers over \uXXXX.
var shortEscapes = map[rune]string{
	'"': `\"`, '\\': `\\`, '\b': `\b`, '\f': `\f`, '\n': `\n`, '\r': `\r`, '\t': `\t`,
}

// appendQuoted appends s as a JSON string. With ascii it escapes every code point outside " "-"~" as
// lowercase \uXXXX, and a code point above U+FFFF as a surrogate pair; without it, only quotes,
// backslashes and control characters are escaped, and lone surrogates stay raw.
//
// py: Modules/_json.c ascii_escape_unicode, escape_unicode (Python 3.15)
func appendQuoted(b []byte, s string, ascii bool) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		// Printable ASCII other than the quote and the backslash needs no escape in either mode.
		plain := i
		for plain < len(s) && ' ' <= s[plain] && s[plain] <= '~' && s[plain] != '"' && s[plain] != '\\' {
			plain++
		}
		if plain > i {
			b, i = append(b, s[i:plain]...), plain
			continue
		}
		r, size := decodeRuneWTF8(s[i:])
		i += size
		if escape, ok := shortEscapes[r]; ok {
			b = append(b, escape...)
			continue
		}
		switch {
		case r < 0x20 || ascii && r > '~' && r < 0x10000:
			b = appendEscape(b, r)
		case ascii && r >= 0x10000:
			r -= 0x10000
			b = appendEscape(appendEscape(b, 0xd800|r>>10), 0xdc00|r&0x3ff)
		default:
			b = appendWTF8(b, r)
		}
	}
	return append(b, '"')
}

// appendEscape appends \uXXXX for a code point below U+10000, with lowercase hex digits.
func appendEscape(b []byte, r rune) []byte {
	const digits = "0123456789abcdef"
	return append(b, '\\', 'u', digits[r>>12&0xf], digits[r>>8&0xf], digits[r>>4&0xf], digits[r&0xf])
}
