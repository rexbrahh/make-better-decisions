package oracletest

import (
	"regexp"
	"strings"
)

var (
	ansi       = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	suggestion = regexp.MustCompile(`, maybe you meant (?:'[^'\n]*'|"[^"\n]*")\?`)
)

// CLIPresentation removes the retired argparse display details from oracle comparisons.
// It leaves runtime output unchanged. Error messages retain whitespace within argument values.
func CLIPresentation(text string) string {
	plain := ansi.ReplaceAllString(text, "")
	if !strings.HasPrefix(plain, "usage: ") {
		return text
	}
	if marker := strings.Index(plain, ": error: "); marker >= 0 {
		start := strings.LastIndexByte(plain[:marker], '\n') + 1
		return suggestion.ReplaceAllString(plain[start:], "")
	}
	return strings.Join(strings.Fields(plain), "")
}
