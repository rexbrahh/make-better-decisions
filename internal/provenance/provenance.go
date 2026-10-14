// Package provenance verifies user quotations. Command output and parent briefs can contain
// agent-authored claims, so only the user's own words count.
//
// py: agent_decisions/provenance.py
package provenance

import (
	"strings"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// Normalize collapses whitespace runs to one space and case-folds text.
//
// py: agent_decisions/provenance.py normalize
func Normalize(text string) string {
	return pystr.Casefold(strings.Join(pystr.Split(text), " "))
}

// Verify splits facts into those quoted from the user's messages and the rest, each in input order. The
// error is the RecursionError that a too deeply nested question-reply envelope raises.
//
// py: agent_decisions/provenance.py verify
func Verify(facts []string, t transcript.Transcript) (verified, unverified []string, err error) {
	var sources []string
	for _, message := range t.UserMessages {
		words, err := transcript.QuestionReplyWords(message)
		if err != nil {
			return nil, nil, err
		}
		if len(words) == 0 {
			words = []string{message}
		}
		for _, said := range words {
			sources = append(sources, Normalize(said))
		}
	}
	corpus := strings.Join(sources, "\x00")
	for _, fact := range facts {
		text := Normalize(fact)
		if pystr.Len(text) >= policy.MinFactChars && strings.Contains(corpus, text) {
			verified = append(verified, fact)
		} else {
			unverified = append(unverified, fact)
		}
	}
	return verified, unverified, nil
}
