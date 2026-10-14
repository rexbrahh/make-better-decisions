// Package str ports the CPython 3.15 str methods that agent-decisions calls and that Go's strings package
// does not reproduce exactly on non-ASCII text.
//
// A Go string stands for the Python str of its code points as DecodeRune reads them, so it may also carry
// lone surrogates and undecodable bytes. Tables come from CPython itself (see tables.go), not from Go's
// unicode package.
//
// The Go counterparts of startswith, endswith, split(sep), rsplit(sep), partition, replace, join and find
// with an ASCII argument are exact already, so this package does not port them.
package str

//go:generate go run ./internal/gentables

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DecodeRune returns the first code point of s and its width in bytes. A 3-byte WTF-8 sequence for a lone
// UTF-16 surrogate (0xED 0xA0-0xBF 0x80-0xBF) is that surrogate. Any other byte that does not start valid
// UTF-8 is one code point, U+DC00 plus the byte, as the surrogateescape error handler decodes it. An empty
// s gives (utf8.RuneError, 0).
func DecodeRune(s string) (rune, int) {
	if s != "" && s[0] < utf8.RuneSelf {
		return rune(s[0]), 1
	}
	r, size := utf8.DecodeRuneInString(s)
	if r != utf8.RuneError || size != 1 {
		return r, size
	}
	if len(s) >= 3 && s[0] == 0xed && s[1] >= 0xa0 && s[1] <= 0xbf && s[2] >= 0x80 && s[2] <= 0xbf {
		return 0xd000 | rune(s[1]&0x3f)<<6 | rune(s[2]&0x3f), 3
	}
	return 0xdc00 + rune(s[0]), 1
}

// Len returns len(s), the number of code points in s.
//
// py: Objects/unicodeobject.c unicode_length (Python 3.15)
func Len(s string) int {
	// Without a 0xED byte, s holds no surrogate sequence, and DecodeRune reads each byte that is not valid
	// UTF-8 as one code point, as utf8.RuneCountInString does.
	if strings.IndexByte(s, 0xed) < 0 {
		return utf8.RuneCountInString(s)
	}
	n := 0
	for i := 0; i < len(s); n++ {
		_, size := DecodeRune(s[i:])
		i += size
	}
	return n
}

// Slice returns s[start:stop]. Negative indices count from the end, and indices outside s are clamped.
//
// py: Objects/unicodeobject.c unicode_subscript (Python 3.15)
func Slice(s string, start, stop int) string {
	n := Len(s)
	start, stop = sliceIndex(start, n), sliceIndex(stop, n)
	if start >= stop {
		return ""
	}
	begin := offset(s, start)
	return s[begin : begin+offset(s[begin:], stop-start)]
}

// SliceFrom returns s[start:].
//
// py: Objects/unicodeobject.c unicode_subscript (Python 3.15)
func SliceFrom(s string, start int) string {
	return Slice(s, start, math.MaxInt)
}

// SliceTo returns s[:stop].
//
// py: Objects/unicodeobject.c unicode_subscript (Python 3.15)
func SliceTo(s string, stop int) string {
	return Slice(s, 0, stop)
}

// sliceIndex adjusts a slice index for a str of n code points, as PySlice_AdjustIndices does for step 1.
func sliceIndex(i, n int) int {
	if i < 0 {
		return max(i+n, 0)
	}
	return min(i, n)
}

// offset returns the byte offset of code point i in s.
func offset(s string, i int) int {
	at := 0
	for ; i > 0; i-- {
		_, size := DecodeRune(s[at:])
		at += size
	}
	return at
}

// IsSpace reports whether the one-character str of r is whitespace.
//
// py: Objects/unicodectype.c _PyUnicode_IsWhitespace (Python 3.15)
func IsSpace(r rune) bool {
	if 0 <= r && r < utf8.RuneSelf {
		return asciiSpace[r]
	}
	return unicode.Is(spaceTable, r)
}

// IsPrintable reports whether the one-character str of r is printable, which also decides whether repr()
// escapes it.
//
// py: Objects/unicodectype.c _PyUnicode_IsPrintable (Python 3.15)
func IsPrintable(r rune) bool {
	return unicode.Is(printableTable, r)
}

// IsAlnum returns s.isalnum(): s is not empty and every code point is alphanumeric.
//
// py: Objects/unicodeobject.c unicode_isalnum_impl (Python 3.15)
func IsAlnum(s string) bool {
	for i := 0; i < len(s); {
		r, size := DecodeRune(s[i:])
		if !unicode.Is(alnumTable, r) {
			return false
		}
		i += size
	}
	return s != ""
}

// The whitespace functions below use Go's decoding: a surrogate or undecodable byte reads as U+FFFD, which,
// like the code point Python sees there, is not whitespace.

// Strip returns s.strip().
//
// py: Objects/unicodeobject.c do_strip (Python 3.15)
func Strip(s string) string {
	return strings.TrimFunc(s, IsSpace)
}

// LStrip returns s.lstrip().
//
// py: Objects/unicodeobject.c do_strip (Python 3.15)
func LStrip(s string) string {
	return strings.TrimLeftFunc(s, IsSpace)
}

// RStrip returns s.rstrip().
//
// py: Objects/unicodeobject.c do_strip (Python 3.15)
func RStrip(s string) string {
	return strings.TrimRightFunc(s, IsSpace)
}

// Split returns s.split(): the runs of non-whitespace code points.
//
// py: Objects/stringlib/split.h split_whitespace (Python 3.15)
func Split(s string) []string {
	return strings.FieldsFunc(s, IsSpace)
}

// StripChars returns s.strip(chars).
//
// py: Objects/unicodeobject.c do_argstrip (Python 3.15)
func StripChars(s, chars string) string {
	return RStripChars(LStripChars(s, chars), chars)
}

// LStripChars returns s.lstrip(chars).
//
// py: Objects/unicodeobject.c do_argstrip (Python 3.15)
func LStripChars(s, chars string) string {
	for s != "" {
		r, size := DecodeRune(s)
		if !containsRune(chars, r) {
			break
		}
		s = s[size:]
	}
	return s
}

// RStripChars returns s.rstrip(chars).
//
// py: Objects/unicodeobject.c do_argstrip (Python 3.15)
func RStripChars(s, chars string) string {
	// A backward decoder can split a surrogate or undecodable byte differently, so this decodes forward. An
	// ASCII byte is always a code point of its own, so a forward decode can start just after one: each
	// round decodes only the trailing run of non-ASCII bytes.
	end := len(s)
	for end > 0 {
		if c := s[end-1]; c < utf8.RuneSelf {
			if !containsRune(chars, rune(c)) {
				return s[:end]
			}
			end--
			continue
		}
		start := end - 1
		for start > 0 && s[start-1] >= utf8.RuneSelf {
			start--
		}
		kept := start
		for i := start; i < end; {
			r, size := DecodeRune(s[i:end])
			if i += size; !containsRune(chars, r) {
				kept = i
			}
		}
		if kept > start {
			return s[:kept]
		}
		end = start
	}
	return ""
}

// containsRune reports whether r is one of the code points of chars.
func containsRune(chars string, r rune) bool {
	if 0 <= r && r < utf8.RuneSelf {
		return strings.IndexByte(chars, byte(r)) >= 0 // an ASCII byte in chars is a code point of its own
	}
	for chars != "" {
		c, size := DecodeRune(chars)
		if c == r {
			return true
		}
		chars = chars[size:]
	}
	return false
}

// SplitLines returns s.splitlines(): the lines of s without their line breaks. "\r\n" is one break.
//
// py: Objects/stringlib/split.h splitlines (Python 3.15)
func SplitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isLineBreak(r) {
			i += size
			continue
		}
		lines = append(lines, s[start:i])
		if r == '\r' && strings.HasPrefix(s[i+1:], "\n") {
			size++
		}
		i += size
		start = i
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// isLineBreak reports whether r ends a line for str.splitlines().
//
// py: Objects/unicodectype.c _PyUnicode_IsLinebreak (Python 3.15)
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// asciiSpace and ascii hold the tables' answers for each ASCII code point, which are the hot path.
var (
	asciiSpace [utf8.RuneSelf]bool
	ascii      [utf8.RuneSelf]struct {
		cased, caseIgnorable bool
		lower                string // what Lower maps the code point to, or "" when it maps to itself
	}
)

func init() {
	for r := range rune(utf8.RuneSelf) {
		asciiSpace[r] = unicode.Is(spaceTable, r)
		c := &ascii[r]
		c.cased, c.caseIgnorable = unicode.Is(casedTable, r), unicode.Is(caseIgnorableTable, r)
		c.lower, _ = caseMapping(lowerRanges, lowerSpecial, r)
	}
}

// caseRange maps the code points lo, lo+stride, ..., hi to themselves plus delta.
type caseRange struct {
	lo, hi, stride, delta rune
}

// caseMapping returns what r maps to under one of the generated case tables, or false when r maps to itself.
func caseMapping(ranges []caseRange, special map[rune]string, r rune) (string, bool) {
	if mapped, ok := special[r]; ok {
		return mapped, true
	}
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].hi >= r })
	if i < len(ranges) && ranges[i].lo <= r && (r-ranges[i].lo)%ranges[i].stride == 0 {
		return string(r + ranges[i].delta), true
	}
	return "", false
}

// mapCodePoints returns s with each code point replaced by what mapping returns for it, if anything. It
// calls mapping once per code point, in order, with the code point's byte offset.
func mapCodePoints(s string, mapping func(at int, r rune) (string, bool)) string {
	var b strings.Builder
	copied := 0
	for i := 0; i < len(s); {
		r, size := DecodeRune(s[i:])
		if mapped, ok := mapping(i, r); ok {
			b.WriteString(s[copied:i])
			b.WriteString(mapped)
			copied = i + size
		}
		i += size
	}
	if copied == 0 {
		return s
	}
	b.WriteString(s[copied:])
	return b.String()
}

// Casefold returns s.casefold(), the full case folding of each code point (for example "ß" to "ss").
//
// py: Objects/unicodeobject.c unicode_casefold_impl (Python 3.15)
func Casefold(s string) string {
	return mapCodePoints(s, func(_ int, r rune) (string, bool) {
		return caseMapping(casefoldRanges, casefoldSpecial, r)
	})
}

// Lower returns s.lower(), the full lowercase mapping of each code point (for example U+0130 to "i̇").
// A capital sigma becomes a final sigma "ς" when a cased code point comes before it and none comes after
// it, skipping case-ignorable code points in both directions.
//
// py: Objects/unicodeobject.c unicode_lower_impl, handle_capital_sigma (Python 3.15)
func Lower(s string) string {
	casedBefore := false // whether the last code point that is not case-ignorable is cased
	return mapCodePoints(s, func(at int, r rune) (string, bool) {
		if r < utf8.RuneSelf {
			c := ascii[r]
			if !c.caseIgnorable {
				casedBefore = c.cased
			}
			return c.lower, c.lower != ""
		}
		final := casedBefore
		if !unicode.Is(caseIgnorableTable, r) {
			casedBefore = unicode.Is(casedTable, r)
		}
		if r != 'Σ' {
			return caseMapping(lowerRanges, lowerSpecial, r)
		}
		if final && !casedFollows(s[at+len("Σ"):]) {
			return "ς", true
		}
		return "σ", true
	})
}

// casedFollows reports whether the first code point of s that is not case-ignorable is cased.
func casedFollows(s string) bool {
	for s != "" {
		r, size := DecodeRune(s)
		if !unicode.Is(caseIgnorableTable, r) {
			return unicode.Is(casedTable, r)
		}
		s = s[size:]
	}
	return false
}

// Repr returns repr(s). It quotes with ' unless s holds ' and no ", escapes the quote, backslash, \t, \n
// and \r, and escapes other code points that are not printable as \xhh, \uhhhh or \Uhhhhhhhh.
//
// py: Objects/unicodeobject.c unicode_repr (Python 3.15)
func Repr(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	b := []byte{quote}
	for i := 0; i < len(s); {
		r, size := DecodeRune(s[i:])
		switch {
		case r == rune(quote) || r == '\\':
			b = append(b, '\\', byte(r))
		case r == '\t':
			b = append(b, `\t`...)
		case r == '\n':
			b = append(b, `\n`...)
		case r == '\r':
			b = append(b, `\r`...)
		case r >= ' ' && r < 0x7f:
			b = append(b, byte(r))
		case r >= 0x80 && IsPrintable(r):
			b = append(b, s[i:i+size]...)
		case r <= 0xff:
			b = fmt.Appendf(b, `\x%02x`, r)
		case r <= 0xffff:
			b = fmt.Appendf(b, `\u%04x`, r)
		default:
			b = fmt.Appendf(b, `\U%08x`, r)
		}
		i += size
	}
	return string(append(b, quote))
}
