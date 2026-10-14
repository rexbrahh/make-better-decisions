// Package fnmatch ports fnmatch.fnmatchcase from CPython 3.15.
//
// CPython translates a pattern to a regular expression with atomic groups, which RE2 lacks. This package
// ports the translation to a list of tokens and matches them directly. Every token but "*" matches exactly
// one code point, so placing each fixed run at its earliest match, as the atomic groups do, cannot miss a
// match, and the classic one-backtrack-point glob matcher gives the same answers.
package fnmatch

import (
	"slices"

	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// span is the code points lo through hi.
type span struct{ lo, hi rune }

// token is one element of a translated pattern. A star token matches any run of code points. Any other
// token matches one code point that is in spans, or, when negate is set, one that is not. So "?" is a
// negated token with no spans, and a set that CPython reduces to nothing is a plain token with no spans.
type token struct {
	star   bool
	negate bool
	spans  []span
}

func (t token) matches(r rune) bool {
	return slices.ContainsFunc(t.spans, func(s span) bool { return s.lo <= r && r <= s.hi }) != t.negate
}

// FnmatchCase reports whether name matches the shell pattern, with case significant. "*" matches any
// run of code points, "?" any one, "[seq]" one in seq and "[!seq]" one not in seq. A "[" without a closing
// "]" is literal, and a backslash is an ordinary character.
//
// py: Lib/fnmatch.py fnmatchcase (Python 3.15)
func FnmatchCase(name, pattern string) bool {
	return match(codePoints(name), translate(codePoints(pattern)))
}

func codePoints(s string) []rune {
	var runes []rune
	for s != "" {
		r, size := str.DecodeRune(s)
		runes = append(runes, r)
		s = s[size:]
	}
	return runes
}

// translate splits pat into tokens.
//
// py: Lib/fnmatch.py _translate (Python 3.15)
func translate(pat []rune) []token {
	var tokens []token
	n := len(pat)
	for i := 0; i < n; {
		c := pat[i]
		i++
		switch c {
		case '*':
			tokens = append(tokens, token{star: true})
			for i < n && pat[i] == '*' {
				i++
			}
		case '?':
			tokens = append(tokens, token{negate: true})
		case '[':
			j := i
			if j < n && pat[j] == '!' {
				j++
			}
			if j < n && pat[j] == ']' {
				j++
			}
			for j < n && pat[j] != ']' {
				j++
			}
			if j >= n {
				tokens = append(tokens, literal('['))
				continue
			}
			tokens = append(tokens, set(pat[i:j]))
			i = j + 1
		default:
			tokens = append(tokens, literal(c))
		}
	}
	return tokens
}

func literal(r rune) token {
	return token{spans: []span{{r, r}}}
}

// set returns the token for the text between "[" and "]". CPython builds a regular expression set from it:
// it cuts the text at the hyphens that make ranges, joins the chunks back with "-" as the range operator,
// escapes every other character, and turns a leading "!" into "^". set reads that regular expression set
// the way Python's re module does.
//
// py: Lib/fnmatch.py _translate (Python 3.15)
func set(stuff []rune) token {
	chunks := [][]rune{stuff}
	if slices.Contains(stuff, '-') {
		chunks = rangeChunks(stuff)
	}
	var t token
	if len(chunks[0]) > 0 && chunks[0][0] == '!' {
		t.negate = true
		chunks[0] = chunks[0][1:]
	}
	// Flatten the set to its characters, marking the range operators between chunks with ok == false.
	type element struct {
		r  rune
		ok bool
	}
	var elements []element
	for k, chunk := range chunks {
		if k > 0 {
			elements = append(elements, element{})
		}
		for _, r := range chunk {
			elements = append(elements, element{r, true})
		}
	}
	for p := 0; p < len(elements); p++ {
		lo := elements[p].r
		if !elements[p].ok {
			// After the "^", re reads a range operator as a literal hyphen. CPython leaves one there
			// when removing a reversed range shortens a leading "!" chunk to "!" alone.
			lo = '-'
		} else if p+2 < len(elements) && !elements[p+1].ok {
			t.spans = append(t.spans, span{lo, elements[p+2].r})
			p += 2
			continue
		}
		t.spans = append(t.spans, span{lo, lo})
	}
	return t
}

// rangeChunks cuts stuff at the hyphens that make ranges, then removes the ranges whose start is after
// their end by joining their two chunks without the range's ends.
//
// py: Lib/fnmatch.py _translate (Python 3.15)
func rangeChunks(stuff []rune) [][]rune {
	var chunks [][]rune
	start, k := 0, 1
	if stuff[0] == '!' {
		k = 2
	}
	for k < len(stuff) {
		hyphen := slices.Index(stuff[k:], '-')
		if hyphen < 0 {
			break
		}
		k += hyphen
		chunks = append(chunks, stuff[start:k])
		start = k + 1
		k += 3
	}
	if chunk := stuff[start:]; len(chunk) > 0 {
		chunks = append(chunks, chunk)
	} else {
		last := len(chunks) - 1
		chunks[last] = slices.Concat(chunks[last], []rune{'-'})
	}
	for k := len(chunks) - 1; k > 0; k-- {
		before, after := chunks[k-1], chunks[k]
		if before[len(before)-1] > after[0] {
			chunks[k-1] = slices.Concat(before[:len(before)-1], after[1:])
			chunks = slices.Delete(chunks, k, k+1)
		}
	}
	return chunks
}

// match reports whether tokens match all of name. After a mismatch it retries from the most recent star
// with that star covering one more code point.
func match(name []rune, tokens []token) bool {
	n, t := 0, 0
	star, starN := -1, 0
	for n < len(name) {
		switch {
		case t < len(tokens) && tokens[t].star:
			star, starN = t, n
			t++
		case t < len(tokens) && tokens[t].matches(name[n]):
			t++
			n++
		case star >= 0:
			starN++
			t, n = star+1, starN
		default:
			return false
		}
	}
	for t < len(tokens) && tokens[t].star {
		t++
	}
	return t == len(tokens)
}
