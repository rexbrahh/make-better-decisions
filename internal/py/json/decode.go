package json

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxDepth is the deepest nesting CPython 3.15's C scanner decodes on the main thread. The scanner checks
// stack use, not a count: each level takes about 128 bytes of the 16 MB main stack, and argv and the
// environment share that stack. `python3 script.py` with a small environment decodes 130696 levels;
// `python3 -c` decodes 130694. The exact limit therefore moves by a few levels per kilobyte of argv and
// environment, and no constant matches every process.
const maxDepth = 130696

// maxIntDigits is sys.int_info.default_max_str_digits, the longest integer literal int() converts.
const maxIntDigits = 4300

// DecodeError is a json.JSONDecodeError. Error returns the same text as str(error).
//
// py: Lib/json/decoder.py JSONDecodeError (Python 3.15)
type DecodeError struct {
	Msg    string // the message without its position, such as "Expecting value"
	Pos    int    // the index of the failure, counted in code points
	Lineno int
	Colno  int
}

func newDecodeError(msg string, text []rune, pos int) *DecodeError {
	lineno, lastNewline := 1, -1
	for i, r := range text[:pos] {
		if r == '\n' {
			lineno, lastNewline = lineno+1, i
		}
	}
	return &DecodeError{Msg: msg, Pos: pos, Lineno: lineno, Colno: pos - lastNewline}
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.Msg, e.Lineno, e.Colno, e.Pos)
}

func (e *DecodeError) PyType() string { return "JSONDecodeError" }

// RecursionError is a Python RecursionError: nesting too deep for CPython's C stack check (Loads, Dumps,
// Equal) or for its recursion limit (LoadsBytes). Python callers that catch ValueError do not catch it.
type RecursionError struct {
	Msg string // str(exception)
}

func (e *RecursionError) Error() string  { return e.Msg }
func (e *RecursionError) PyType() string { return "RecursionError" }

// stackOverflow returns the C stack check's RecursionError on CPython's 16 MB main-thread stack; where
// names the operation, such as " in comparison".
//
// py: Python/ceval.c _Py_CheckRecursiveCall (Python 3.15)
func stackOverflow(where string) *RecursionError {
	return &RecursionError{"Stack overflow (used 16352 kB)" + where}
}

// IntDigitsError is the plain ValueError, not a JSONDecodeError, that int() raises for an integer literal
// longer than 4300 digits.
//
// py: Objects/longobject.c PyLong_FromString (Python 3.15)
type IntDigitsError struct {
	Digits int
}

func (e *IntDigitsError) Error() string {
	return fmt.Sprintf("Exceeds the limit (%d digits) for integer string conversion: value has %d digits; "+
		"use sys.set_int_max_str_digits() to increase the limit", maxIntDigits, e.Digits)
}

func (e *IntDigitsError) PyType() string { return "ValueError" }

// Loads ports json.loads(s) for a str with default arguments. It returns a *DecodeError for malformed JSON,
// an *IntDigitsError for an over-long integer, and a *RecursionError for too deep nesting. Lone surrogate
// bytes in s pass through; any other invalid UTF-8 byte reads as U+FFFD.
//
// py: Lib/json/__init__.py loads, Lib/json/decoder.py JSONDecoder.decode (Python 3.15)
func Loads(s string) (any, error) {
	text := decodeWTF8(s)
	if len(text) > 0 && text[0] == 0xfeff {
		return nil, newDecodeError("Unexpected UTF-8 BOM (decode using utf-8-sig)", text, 0)
	}
	scanner := &scanner{text: text}
	value, end, err := scanner.scanOnce(scanner.skipWhitespace(0))
	if err != nil {
		return nil, err
	}
	if end = scanner.skipWhitespace(end); end != len(text) {
		return nil, scanner.fail("Extra data", end)
	}
	return value, nil
}

// scanner holds one document as code points, so every index matches the Python str index.
type scanner struct {
	text  []rune
	depth int
}

func (s *scanner) fail(msg string, pos int) error {
	return newDecodeError(msg, s.text, pos)
}

// at returns the code point at idx, or -1 past the end.
func (s *scanner) at(idx int) rune {
	if idx < len(s.text) {
		return s.text[idx]
	}
	return -1
}

func (s *scanner) skipWhitespace(idx int) int {
	for r := s.at(idx); r == ' ' || r == '\t' || r == '\n' || r == '\r'; r = s.at(idx) {
		idx++
	}
	return idx
}

// constants are the words scan_once_unicode matches before it tries a number.
var constants = []struct {
	word  string
	value any
}{
	{"null", nil},
	{"true", true},
	{"false", false},
	{"NaN", math.NaN()},
	{"Infinity", math.Inf(1)},
	{"-Infinity", math.Inf(-1)},
}

// scanOnce decodes the value that starts at idx and returns it with the index after it. Python raises
// StopIteration(idx) here and raw_decode turns it into "Expecting value" at idx, so this fails directly.
//
// py: Modules/_json.c scan_once_unicode (Python 3.15)
func (s *scanner) scanOnce(idx int) (any, int, error) {
	switch s.at(idx) {
	case -1:
		return nil, 0, s.fail("Expecting value", idx)
	case '"':
		value, end, err := s.scanString(idx + 1)
		return value, end, err
	case '{':
		if s.depth == maxDepth {
			return nil, 0, stackOverflow(" while decoding a JSON object from a unicode string")
		}
		s.depth++
		value, end, err := s.parseObject(idx + 1)
		s.depth--
		return value, end, err
	case '[':
		if s.depth == maxDepth {
			return nil, 0, stackOverflow(" while decoding a JSON array from a unicode string")
		}
		s.depth++
		value, end, err := s.parseArray(idx + 1)
		s.depth--
		return value, end, err
	}
	for _, constant := range constants {
		end := idx + len(constant.word)
		if end <= len(s.text) && string(s.text[idx:end]) == constant.word {
			return constant.value, end, nil
		}
	}
	return s.matchNumber(idx)
}

// parseObject decodes the members of an object whose "{" is just before idx.
//
// py: Modules/_json.c _parse_object_unicode (Python 3.15)
func (s *scanner) parseObject(idx int) (any, int, error) {
	object := &Object{}
	idx = s.skipWhitespace(idx)
	if s.at(idx) == '}' {
		return object, idx + 1, nil
	}
	for {
		if s.at(idx) != '"' {
			return nil, 0, s.fail("Expecting property name enclosed in double quotes", idx)
		}
		key, end, err := s.scanString(idx + 1)
		if err != nil {
			return nil, 0, err
		}
		if idx = s.skipWhitespace(end); s.at(idx) != ':' {
			return nil, 0, s.fail("Expecting ':' delimiter", idx)
		}
		value, end, err := s.scanOnce(s.skipWhitespace(idx + 1))
		if err != nil {
			return nil, 0, err
		}
		object.Set(key, value)
		idx = s.skipWhitespace(end)
		if s.at(idx) == '}' {
			return object, idx + 1, nil
		}
		if s.at(idx) != ',' {
			return nil, 0, s.fail("Expecting ',' delimiter", idx)
		}
		comma := idx
		if idx = s.skipWhitespace(idx + 1); s.at(idx) == '}' {
			return nil, 0, s.fail("Illegal trailing comma before end of object", comma)
		}
	}
}

// parseArray decodes the items of an array whose "[" is just before idx.
//
// py: Modules/_json.c _parse_array_unicode (Python 3.15)
func (s *scanner) parseArray(idx int) (any, int, error) {
	list := []any{}
	idx = s.skipWhitespace(idx)
	if s.at(idx) == ']' {
		return list, idx + 1, nil
	}
	for {
		value, end, err := s.scanOnce(idx)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, value)
		idx = s.skipWhitespace(end)
		if s.at(idx) == ']' {
			return list, idx + 1, nil
		}
		if s.at(idx) != ',' {
			return nil, 0, s.fail("Expecting ',' delimiter", idx)
		}
		comma := idx
		if idx = s.skipWhitespace(idx + 1); s.at(idx) == ']' {
			return nil, 0, s.fail("Illegal trailing comma before end of array", comma)
		}
	}
}

// backslashEscapes maps the character after a backslash to the character it stands for, except "u".
var backslashEscapes = map[rune]rune{
	'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t',
}

// scanString decodes a string whose opening quote is just before end. It ports the C scanner, whose
// positions and messages differ from the pure-Python py_scanstring.
//
// py: Modules/_json.c scanstring_unicode (Python 3.15)
func (s *scanner) scanString(end int) (string, int, error) {
	text := s.text
	begin := end - 1
	var decoded []byte
	for {
		next := end
		for next < len(text) && text[next] != '"' && text[next] != '\\' {
			if text[next] <= 0x1f {
				return "", 0, s.fail("Invalid control character at", next)
			}
			next++
		}
		for _, r := range text[end:next] {
			decoded = appendWTF8(decoded, r)
		}
		if next == len(text) {
			return "", 0, s.fail("Unterminated string starting at", begin)
		}
		if text[next] == '"' {
			return string(decoded), next + 1, nil
		}
		next++ // the character after the backslash
		if next == len(text) {
			return "", 0, s.fail("Unterminated string starting at", begin)
		}
		if text[next] != 'u' {
			r, ok := backslashEscapes[text[next]]
			if !ok {
				return "", 0, s.fail("Invalid \\escape", next-1)
			}
			decoded = append(decoded, byte(r))
			end = next + 1
			continue
		}
		end = next + 5
		if end > len(text) {
			return "", 0, s.fail("Invalid \\uXXXX escape", next)
		}
		r, ok := hex4(text[next+1 : end])
		if !ok {
			return "", 0, s.fail("Invalid \\uXXXX escape", next)
		}
		if 0xd800 <= r && r <= 0xdbff && end+6 < len(text) && text[end] == '\\' && text[end+1] == 'u' {
			low, ok := hex4(text[end+2 : end+6])
			if !ok {
				return "", 0, s.fail("Invalid \\uXXXX escape", end+1)
			}
			if 0xdc00 <= low && low <= 0xdfff {
				r = 0x10000 + (r-0xd800)<<10 + (low - 0xdc00)
				end += 6
			}
		}
		decoded = appendWTF8(decoded, r)
	}
}

// hex4 parses four hex digits of either case.
func hex4(digits []rune) (rune, bool) {
	var r rune
	for _, digit := range digits {
		switch {
		case '0' <= digit && digit <= '9':
			r = r<<4 | (digit - '0')
		case 'a' <= digit && digit <= 'f':
			r = r<<4 | (digit - 'a' + 10)
		case 'A' <= digit && digit <= 'F':
			r = r<<4 | (digit - 'A' + 10)
		default:
			return 0, false
		}
	}
	return r, true
}

func isDigit(r rune) bool {
	return '0' <= r && r <= '9'
}

// matchNumber decodes the number at start. A number with a fraction or an exponent is a float; any other
// is an int. Like the regex -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?, it stops before a "." or "e"
// that no digit follows.
//
// py: Modules/_json.c _match_number_unicode (Python 3.15)
func (s *scanner) matchNumber(start int) (any, int, error) {
	idx := start
	if s.at(idx) == '-' {
		idx++
	}
	switch r := s.at(idx); {
	case r == '0':
		idx++
	case isDigit(r):
		for idx++; isDigit(s.at(idx)); idx++ {
		}
	default:
		return nil, 0, s.fail("Expecting value", start)
	}
	isFloat := false
	if s.at(idx) == '.' && isDigit(s.at(idx+1)) {
		isFloat = true
		for idx += 2; isDigit(s.at(idx)); idx++ {
		}
	}
	if r := s.at(idx); r == 'e' || r == 'E' {
		exponent := idx + 1
		if r := s.at(exponent); r == '-' || r == '+' {
			exponent++
		}
		if isDigit(s.at(exponent)) {
			isFloat = true
			for idx = exponent + 1; isDigit(s.at(idx)); idx++ {
			}
		}
	}
	literal := string(s.text[start:idx])
	if isFloat {
		// The literal is valid float syntax, so the only possible error is ErrRange, which comes with the
		// same ±Inf that float() returns.
		f, _ := strconv.ParseFloat(literal, 64)
		return f, idx, nil
	}
	digits := strings.TrimPrefix(literal, "-")
	if len(digits) > maxIntDigits {
		return nil, 0, &IntDigitsError{Digits: len(digits)}
	}
	if digits == "0" {
		return Int("0"), idx, nil
	}
	return Int(literal), idx, nil
}

// surrogateEscape matches a JSON escape of a surrogate code point.
var surrogateEscape = regexp.MustCompile(`\\u[dD][89a-fA-F]`)

// recursionLimit is sys.getrecursionlimit(), the default 1000: a call that would make the Python stack
// deeper than this many frames raises RecursionError.
const recursionLimit = 1000

// LoadsBytes ports agent_decisions.transcript.loads: json.loads(data.decode("utf-8", errors="replace")),
// and when data holds a surrogate escape, every surrogate in every key and string becomes U+FFFD. The
// result always encodes as UTF-8.
//
// The surrogate pass recurses once per nesting level plus once for each leaf, in Python frames. frames is
// the depth of the Python stack at the loads call, counting the loads frame and the script's <module>
// frame: 3 for hook.py's main, which loads stdin. With frames 4, 995 nested lists around a string pass and
// 996 raise RecursionError (CPython 3.15, measured).
//
// py: agent_decisions/transcript.py loads (Python 3.15)
func LoadsBytes(data []byte, frames int) (any, error) {
	value, err := Loads(DecodeUTF8(data))
	if err != nil || !surrogateEscape.Match(data) {
		return value, err
	}
	return withoutSurrogates(value, frames+1)
}

// withoutSurrogates returns value with each surrogate code point in its strings replaced by U+FFFD. Two
// keys that become equal collapse into one, as in the Python dict comprehension. frame is the Python stack
// depth of this call.
//
// py: agent_decisions/transcript.py _without_surrogates (Python 3.15)
func withoutSurrogates(value any, frame int) (any, error) {
	if frame > recursionLimit {
		return nil, &RecursionError{"maximum recursion depth exceeded"}
	}
	switch value := value.(type) {
	case string:
		var replaced []byte
		for _, r := range decodeWTF8(value) {
			if isSurrogate(r) {
				r = utf8.RuneError
			}
			replaced = appendWTF8(replaced, r)
		}
		return string(replaced), nil
	case []any:
		list := make([]any, len(value))
		for i, item := range value {
			replaced, err := withoutSurrogates(item, frame+1)
			if err != nil {
				return nil, err
			}
			list[i] = replaced
		}
		return list, nil
	case *Object:
		object := &Object{}
		for _, key := range value.keys {
			replacedKey, err := withoutSurrogates(key, frame+1)
			if err != nil {
				return nil, err
			}
			replaced, err := withoutSurrogates(value.values[key], frame+1)
			if err != nil {
				return nil, err
			}
			object.Set(replacedKey.(string), replaced)
		}
		return object, nil
	}
	return value, nil
}

// DecodeUTF8 returns data.decode("utf-8", errors="replace"). Each maximal invalid subpart becomes one
// U+FFFD, and encoded surrogates (0xED 0xA0-0xBF) are invalid, as in CPython. Go's utf8 package replaces
// each bad byte on its own, which differs for truncated sequences.
//
// py: Objects/unicodeobject.c unicode_decode_utf8 (Python 3.15)
func DecodeUTF8(data []byte) string {
	var decoded strings.Builder
	decoded.Grow(len(data))
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			decoded.WriteRune(utf8.RuneError)
			i += invalidPrefixLength(data[i:])
			continue
		}
		decoded.Write(data[i : i+size])
		i += size
	}
	return decoded.String()
}

// invalidPrefixLength returns the length of the maximal subpart at the start of data, which does not begin
// with a valid UTF-8 sequence: the lead byte plus the continuation bytes that could still have completed it.
//
// py: Objects/stringlib/codecs.h utf8_decode (Python 3.15)
func invalidPrefixLength(data []byte) int {
	lead := data[0]
	var size int
	var low, high byte = 0x80, 0xbf // the allowed range of the second byte
	switch {
	case 0xc2 <= lead && lead <= 0xdf:
		size = 2
	case lead == 0xe0:
		size, low = 3, 0xa0
	case lead == 0xed:
		size, high = 3, 0x9f
	case 0xe1 <= lead && lead <= 0xef:
		size = 3
	case lead == 0xf0:
		size, low = 4, 0x90
	case lead == 0xf4:
		size, high = 4, 0x8f
	case 0xf1 <= lead && lead <= 0xf3:
		size = 4
	default:
		return 1
	}
	if len(data) < 2 || data[1] < low || data[1] > high {
		return 1
	}
	n := 2
	for n < size && n < len(data) && 0x80 <= data[n] && data[n] <= 0xbf {
		n++
	}
	return n
}

func isSurrogate(r rune) bool {
	return 0xd800 <= r && r <= 0xdfff
}

// decodeWTF8 returns the code points of s. A 3-byte encoded surrogate decodes to that surrogate; any other
// invalid byte decodes to U+FFFD.
func decodeWTF8(s string) []rune {
	runes := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := decodeRuneWTF8(s[i:])
		runes = append(runes, r)
		i += size
	}
	return runes
}

// decodeRuneWTF8 decodes the first code point of s like utf8.DecodeRuneInString, but also accepts a
// 3-byte encoded surrogate.
func decodeRuneWTF8(s string) (rune, int) {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 && len(s) >= 3 && s[0] == 0xed &&
		0xa0 <= s[1] && s[1] <= 0xbf && 0x80 <= s[2] && s[2] <= 0xbf {
		return 0xd000 | rune(s[1]&0x3f)<<6 | rune(s[2]&0x3f), 3
	}
	return r, size
}

// appendWTF8 appends the UTF-8 encoding of r, writing a surrogate as its 3-byte encoding where
// utf8.AppendRune would write U+FFFD.
func appendWTF8(b []byte, r rune) []byte {
	if isSurrogate(r) {
		return append(b, 0xed, byte(0x80|(r>>6)&0x3f), byte(0x80|r&0x3f))
	}
	return utf8.AppendRune(b, r)
}
