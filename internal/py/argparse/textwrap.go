package argparse

import "strings"

// The subset of CPython 3.15's textwrap that HelpFormatter._split_lines uses: textwrap.wrap(text, width)
// with the default TextWrapper, for text that _split_lines already collapsed to single spaces.

// isWord reports whether b is in \w for ASCII text.
func isWord(b byte) bool {
	return isLetter(b) || '0' <= b && b <= '9'
}

// isLetter reports whether b is in [^\d\W] for ASCII text: a letter or "_".
func isLetter(b byte) bool {
	return 'a' <= b|0x20 && b|0x20 <= 'z' || b == '_'
}

// isWordPunct reports whether b is in TextWrapper's word_punct, [\w!"'&.,?], for ASCII text.
func isWordPunct(b byte) bool {
	return isWord(b) || strings.IndexByte(`!"'&.,?`, b) >= 0
}

// wrapChunks returns TextWrapper._split(text) with break_on_hyphens: the matches of wordsep_re, which
// cover the whole text.
// ponytail: ASCII text whose only whitespace is single spaces, as _split_lines leaves it; non-ASCII
// letters would need the Unicode \w table.
//
// py: Lib/textwrap.py TextWrapper.wordsep_re, TextWrapper._split (Python 3.15)
func wrapChunks(text string) []string {
	at := func(i int, test func(byte) bool) bool { return i >= 0 && i < len(text) && test(text[i]) }
	isDash := func(b byte) bool { return b == '-' }
	// emDash reports whether -{2,} followed by \w starts at i.
	emDash := func(i int) bool {
		j := i
		for at(j, isDash) {
			j++
		}
		return j-i >= 2 && at(j, isWord)
	}
	var chunks []string
	for i := 0; i < len(text); {
		end := i + 1
		switch {
		case text[i] == ' ':
			for at(end, func(b byte) bool { return b == ' ' }) {
				end++
			}
		case at(i-1, isWordPunct) && emDash(i): // the em-dash between words
			for at(end, isDash) {
				end++
			}
		default: // a word up to a hyphen, the end of the word, or an em-dash, whichever comes first
			for j := i + 1; ; j++ {
				hyphen := at(j, isDash) &&
					(at(j-2, isLetter) && at(j-1, isLetter) || at(j-3, isLetter) && at(j-2, isDash) && at(j-1, isLetter)) &&
					at(j+1, isLetter) && (at(j+2, isLetter) || at(j+2, isDash) && at(j+3, isLetter))
				if hyphen {
					end = j + 1
					break
				}
				if j == len(text) || text[j] == ' ' || at(j-1, isWordPunct) && emDash(j) {
					end = j
					break
				}
			}
		}
		chunks, i = append(chunks, text[i:end]), end
	}
	return chunks
}

// wrap returns textwrap.wrap(text, width) for ASCII text with single spaces.
//
// py: Lib/textwrap.py TextWrapper._wrap_chunks, _handle_long_word (Python 3.15)
func wrap(text string, width int) []string {
	chunks := wrapChunks(text)
	var lines []string
	for len(chunks) > 0 {
		if chunks[0] == " " && len(lines) > 0 {
			chunks = chunks[1:]
		}
		var line []string
		length := 0
		for len(chunks) > 0 && length+len(chunks[0]) <= width {
			line, length, chunks = append(line, chunks[0]), length+len(chunks[0]), chunks[1:]
		}
		if len(chunks) > 0 && len(chunks[0]) > width {
			// _handle_long_word with break_long_words and break_on_hyphens
			spaceLeft := width - length
			if width < 1 {
				spaceLeft = 1
			}
			switch chunk := chunks[0]; {
			case spaceLeft > 0:
				end := spaceLeft
				if len(chunk) > spaceLeft {
					if hyphen := strings.LastIndex(chunk[:spaceLeft], "-"); hyphen > 0 && strings.Trim(chunk[:hyphen], "-") != "" {
						end = hyphen + 1
					}
				}
				line, chunks[0] = append(line, chunk[:end]), chunk[end:]
			case len(line) == 0:
				line, chunks = append(line, chunk), chunks[1:]
			}
		}
		if len(line) > 0 && line[len(line)-1] == " " {
			line = line[:len(line)-1]
		}
		if len(line) > 0 {
			lines = append(lines, strings.Join(line, ""))
		}
	}
	return lines
}
