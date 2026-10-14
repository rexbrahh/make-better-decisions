// Package re holds Go regexp bracket expressions equal to the Unicode-aware classes of CPython 3.15's re
// module on str patterns. RE2's \s, \w and \d are ASCII-only, and its \s also lacks \v. Splice Space,
// NotSpace, Word, NotWord, Digit and NotDigit (classes.go) into a Go expression where the Python pattern
// has \s, \S, \w, \W, \d and \D.
//
// Port these other differences by hand. Each was checked against CPython 3.15 and Go's regexp package:
//
//   - \b and \B use Unicode word characters in Python and ASCII ones in RE2: Python's \bé finds "é" in " é",
//     RE2's does not.
//   - Python's $ without (?m) matches at the end and also before a final "\n", so a$ finds "a" in "a\n".
//     RE2's $ without (?m) matches only at the end.
//   - Python's \Z and \z both match only at the end. RE2 has \z and rejects \Z.
//   - RE2 rejects lookahead, lookbehind, backreferences, atomic groups and possessive quantifiers.
//   - Python reads a{,2} as a{0,2}. RE2 reads it as the literal text "a{,2}".
//   - re.match anchors at the start only, re.fullmatch at both ends, and re.search nowhere. Go's MatchString
//     and Find methods search, so write \A(?:...) for re.match and \A(?:...)\z for re.fullmatch.
//   - (?s) and (?m) have the same meaning in both, including where (?m)^ and (?m)$ match around a final "\n".
//   - With IGNORECASE, Python also matches "i" and "I" with "ı" (U+0131) and "İ" (U+0130). RE2's (?i)
//     does not. Neither matches "ss" with "ß".
//   - RE2 reads each byte of a lone surrogate or invalid UTF-8 as U+FFFD, which no positive class here
//     contains. Its match offsets count bytes; Python's count code points.
package re
