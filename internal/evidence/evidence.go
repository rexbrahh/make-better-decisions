// Package evidence builds the evidence Jev sees: user words, labelled tool observations, and source reads.
//
// Python frames: CPython raises RecursionError when a call would make the Python stack deeper than 1000
// frames. scrub and _walk_strings recurse once per nesting level of a JSON value, so the functions that
// reach them take frame, the depth of the Python stack at the Python call they port (counting that call's
// own frame and the <module> frame).
//
// py: agent_decisions/evidence.py
package evidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	pyre "github.com/rexbrahh/make-better-decisions/internal/py/re"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// Redacted replaces each secret in evidence text.
//
// py: agent_decisions/evidence.py REDACTED
const Redacted = "[REDACTED]"

// recursionLimit is sys.getrecursionlimit(), the default 1000: a call that would make the Python stack
// deeper raises RecursionError.
const recursionLimit = 1000

// spaceChars is the inside of re.Space, to splice Python's \s into a larger bracket expression.
var spaceChars = pyre.Space[1 : len(pyre.Space)-1]

// finder returns the byte offsets of the Python match that a search from byte offset from finds in text,
// with the submatch pairs of FindStringSubmatchIndex, or nil when there is none. A match is never empty.
type finder func(text string, from int) []int

// The IGNORECASE patterns spell out "\x{130}\x{131}" wherever an "i" or "I" can match: Python's re matches
// both with "İ" and "ı" under IGNORECASE, and RE2's (?i) does not. Both match "k" with U+212A and "s" with
// U+017F already.
var (
	// py: agent_decisions/evidence.py _SECRETS
	secretPatterns = []finder{
		// py: (?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}
		prefiltered(wordBounded(regexp.MustCompile(`(?i)bearer`+pyre.Space+`+[A-Za-z0-9._~+/=\x{130}\x{131}-]{16,}`), false),
			true, "bearer"),
		// py: \b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}
		prefiltered(wordBounded(regexp.MustCompile(`(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}`), false), false, "k-"),
		// py: \b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}
		wordBounded(regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}`), false),
		// py: \bxox[abprs]-[A-Za-z0-9-]{10,}
		wordBounded(regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`), false),
		// py: \bAKIA[0-9A-Z]{16}\b
		wordBounded(regexp.MustCompile(`AKIA[0-9A-Z]{16}`), true),
		// py: \beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}
		wordBounded(regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), false),
		findPrivateKey,
	}

	// py: agent_decisions/evidence.py _ASSIGNMENT
	// py: (?i)\b([A-Z0-9_]*(?:API_KEY|SECRET|TOKEN|PASSWORD|PASSWD)[A-Z0-9_]*)(['\"]?\s*[=:]\s*)(['\"]?)[^\s'\"]{8,}
	assignment = prefiltered(wordBounded(regexp.MustCompile(`(?i)([A-Z0-9_\x{130}\x{131}]*`+
		`(?:AP[I\x{130}\x{131}]_KEY|SECRET|TOKEN|PASSWORD|PASSWD)[A-Z0-9_\x{130}\x{131}]*)`+
		`(['"]?`+pyre.Space+`*[=:]`+pyre.Space+`*)(['"]?)[^`+spaceChars+`'"]{8,}`), false),
		true, "api_key", "secret", "token", "passw")

	// pathToken is the group and lookahead of _PATH_TOKEN, anchored at the start; pathTokens does the
	// rest. The lookahead (?=$|[...]) becomes a consumed character or the end, which accepts the same
	// group: Python's $ also matches before a final "\n", and \s already holds "\n".
	//
	// py: agent_decisions/evidence.py _PATH_TOKEN
	pathToken = regexp.MustCompile(`\A(` +
		`(?:\.{1,2}/|/)?[A-Za-z0-9_@+.-]+(?:/[A-Za-z0-9_@+.-]+)+` +
		`|[A-Za-z0-9_@+.-]+\.(?:c|cc|cpp|cxx|css|go|h|hpp|html|ini|js|json|jsx|md|py|rs|sh|toml|ts|tsx|txt|yaml|yml)` +
		`|\.env(?:\.[A-Za-z0-9_.-]+)?)` +
		`(?:[` + spaceChars + `"'` + "`" + `)>}\],:;?!]|\z)`)
	// pathTokenStart is the prefix class (?:^|[\s\"'`(<{\[]) of _PATH_TOKEN without ^.
	pathTokenStart = regexp.MustCompile(`\A[` + spaceChars + `"'` + "`" + `(<{\[]`)
	// asciiPathTokenStart says, for each ASCII character, whether pathTokenStart matches it.
	asciiPathTokenStart = func() (table [utf8.RuneSelf]bool) {
		for c := range table {
			table[c] = pathTokenStart.MatchString(string(rune(c)))
		}
		return table
	}()

	// lineSuffix is the pattern of _clean_path_candidate's re.sub without its $, which cleanPathCandidate
	// ports by hand.
	//
	// py: agent_decisions/evidence.py _clean_path_candidate :\d+(?::\d+)?$
	lineSuffix = regexp.MustCompile(`:` + pyre.Digit + `+(?::` + pyre.Digit + `+)?\z`)

	// py: agent_decisions/evidence.py _git_revision [a-f0-9]{40,64}
	gitRevisionPattern = regexp.MustCompile(`\A[a-f0-9]{40,64}\z`)

	// privateKeyBegin and privateKeyEnd are the two ends of the last _SECRETS pattern (see findPrivateKey).
	privateKeyBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	privateKeyEnd   = regexp.MustCompile(`\A-----END [A-Z ]*PRIVATE KEY-----`)
)

// py: agent_decisions/evidence.py _CONSTRAINT_HINTS ... _MAX_SOURCE_QUERY_TERMS
var (
	constraintHints = []string{
		"must", "never", "always", "only", "preserve", "do not", "don't", "scope",
		"constraint", "required", "require", "non negotiable", "approved", "without authorization",
	}
	ruleFiles       = []string{"AGENTS.md", "CLAUDE.md"}
	privateParts    = map[string]bool{".git": true, ".hg": true, ".svn": true, "secrets": true, "secret": true, "keys": true}
	privateNames    = map[string]bool{"id_rsa": true, "id_ed25519": true, "known_hosts": true}
	privateSuffixes = []string{".key", ".pem", ".p12", ".pfx"}
)

const (
	maxSourceBytes           = 1_000_000
	maxSourceFiles           = 8
	maxSourceCandidateProbes = 16
	contextMetadataChars     = 256 // counts and hook.py's signed-history label
	maxSourceQueryTerms      = 16
	gitTimeout               = time.Second
	maxPathCandidateChars    = 1_024
)

// normalizeEvidenceProfile accepts only the fixed policy profiles. A zero profile preserves the Jev
// defaults for callers that predate provider-specific evidence sizing.
func normalizeEvidenceProfile(profile policy.EvidenceProfile) (policy.EvidenceProfile, error) {
	jev, err := policy.Provider("")
	if err != nil {
		return policy.EvidenceProfile{}, err
	}
	subq, err := policy.Provider("subq")
	if err != nil {
		return policy.EvidenceProfile{}, err
	}
	if profile == (policy.EvidenceProfile{}) {
		return jev.Evidence, nil
	}
	if profile == jev.Evidence || profile == subq.Evidence {
		return profile, nil
	}
	return policy.EvidenceProfile{}, fmt.Errorf("unsupported evidence profile budget %d text %d",
		profile.BudgetChars, profile.TextChars)
}

// wordBounded returns the finder of the Python pattern \b(pattern), or \b(pattern)\b when after is set,
// where every match of pattern starts and ends with a word character. RE2's \b is ASCII-only, so this
// checks Python's Unicode \b by hand: the code point before the match, and the one after it when after is
// set, must not be a word character. A failed check moves the search one code point on, as Python's search
// does.
func wordBounded(pattern *regexp.Regexp, after bool) finder {
	return func(text string, from int) []int {
		for from < len(text) {
			loc := pattern.FindStringSubmatchIndex(text[from:])
			if loc == nil {
				return nil
			}
			for i := range loc {
				if loc[i] >= 0 {
					loc[i] += from
				}
			}
			before, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
			next, _ := utf8.DecodeRuneInString(text[loc[1]:])
			if (loc[0] == 0 || !isWord(before)) && (!after || loc[1] == len(text) || !isWord(next)) {
				return loc
			}
			_, size := utf8.DecodeRuneInString(text[loc[0]:])
			from = loc[0] + size
		}
		return nil
	}
}

// caseFolds maps the code points that Python's IGNORECASE matches with an ASCII letter of a keyword
// (K, S, I) to that letter, so a lower-cased copy holds every keyword that the pattern can match.
var caseFolds = strings.NewReplacer("\u212a", "k", "\u017f", "s", "\u0130", "i", "\u0131", "i")

// prefiltered returns find, which first checks that text holds one of the literals that every match
// contains (case-folded when fold is set), and finds nothing without one. RE2 has no literal prefix to
// skip ahead with on these patterns and scans a 2 MB tool output in up to 170 ms; the check costs a few.
// It runs once per sub, on the search from the start.
func prefiltered(find finder, fold bool, literals ...string) finder {
	return func(text string, from int) []int {
		if from == 0 {
			probe := text
			if fold {
				probe = strings.ToLower(caseFolds.Replace(text))
			}
			if !slices.ContainsFunc(literals, func(literal string) bool { return strings.Contains(probe, literal) }) {
				return nil
			}
		}
		return find(text, from)
	}
}

// isWord reports whether Python's \w matches r in a str pattern. Go decodes a lone surrogate or a stray
// byte as U+FFFD, which, like the code point Python sees there, is not a word character.
func isWord(r rune) bool {
	return r == '_' || pystr.IsAlnum(string(r))
}

// findPrivateKey is the finder of the last _SECRETS pattern. Its lazy body stops at the first
// "-----END [A-Z ]*PRIVATE KEY-----" and fails at a "-----BEGIN " that comes first. Both header runs of
// [A-Z ]* stop at "-", so each header matches in at most one way where it starts.
//
// py: -----BEGIN [A-Z ]*PRIVATE KEY-----(?:(?!-----BEGIN )[\s\S])*?-----END [A-Z ]*PRIVATE KEY-----
func findPrivateKey(text string, from int) []int {
	for from < len(text) {
		loc := privateKeyBegin.FindStringIndex(text[from:])
		if loc == nil {
			return nil
		}
		start, body := from+loc[0], from+loc[1]
		stop := len(text)
		if i := strings.Index(text[body:], "-----BEGIN "); i >= 0 {
			stop = body + i
		}
		for p := body; p < stop; {
			i := strings.Index(text[p:stop], "-----END ")
			if i < 0 {
				break
			}
			if end := privateKeyEnd.FindStringIndex(text[p+i:]); end != nil {
				return []int{start, p + i + end[1]}
			}
			p += i + 1
		}
		from = start + 1 // the header starts with an ASCII "-"
	}
	return nil
}

// sub returns pattern.sub(replacement, text) for a finder, and whether any match was replaced.
func sub(text string, find finder, replacement func(loc []int) string) (string, bool) {
	var out strings.Builder
	done, matched := 0, false
	for loc := find(text, 0); loc != nil; loc = find(text, loc[1]) {
		out.WriteString(text[done:loc[0]])
		out.WriteString(replacement(loc))
		done, matched = loc[1], true
	}
	if !matched {
		return text, false
	}
	out.WriteString(text[done:])
	return out.String(), true
}

// Redact returns text with each known secret of 16 or more characters and each secret-shaped token
// replaced by Redacted.
//
// py: agent_decisions/evidence.py redact
func Redact(text string, secrets []string) string {
	text, _ = redact(text, secrets)
	return text
}

// redact is Redact, and also reports whether the _ASSIGNMENT replacement lambda ran: that call is one more
// Python frame.
func redact(text string, secrets []string) (string, bool) {
	for _, secret := range secrets {
		if pystr.Len(secret) >= 16 {
			text = strings.ReplaceAll(text, secret, Redacted) // exact on WTF-8: no code point splits
		}
	}
	for _, find := range secretPatterns {
		text, _ = sub(text, find, func([]int) string { return Redacted })
	}
	return sub(text, assignment, func(m []int) string {
		return text[m[2]:m[3]] + text[m[4]:m[5]] + text[m[6]:m[7]] + Redacted
	})
}

// Scrub returns value with Redact applied to every string in it. frame is the depth of the Python stack at
// the scrub call; a value nested too deep for it is a RecursionError, as in Python.
//
// py: agent_decisions/evidence.py scrub
func Scrub(value any, secrets []string, frame int) (any, error) {
	if frame > recursionLimit {
		return nil, recursionError()
	}
	switch value := value.(type) {
	case string:
		if frame+1 > recursionLimit { // redact's frame
			return nil, recursionError()
		}
		text, called := redact(value, secrets)
		if called && frame+2 > recursionLimit { // the frame of redact's replacement lambda
			return nil, recursionError()
		}
		return text, nil
	case []any:
		list := make([]any, len(value))
		for i, item := range value {
			scrubbed, err := Scrub(item, secrets, frame+1)
			if err != nil {
				return nil, err
			}
			list[i] = scrubbed
		}
		return list, nil
	case *json.Object:
		object := &json.Object{}
		for _, key := range value.Keys() {
			scrubbed, err := Scrub(value.Value(key), secrets, frame+1)
			if err != nil {
				return nil, err
			}
			object.Set(key, scrubbed)
		}
		return object, nil
	}
	return value, nil
}

func recursionError() error {
	return &json.RecursionError{Msg: "maximum recursion depth exceeded"}
}

// Clip returns text when it has at most limit code points, and otherwise its head and tail with a note of
// how many code points were cut.
//
// py: agent_decisions/evidence.py clip
func Clip(text string, limit int) string {
	n := pystr.Len(text)
	if n <= limit {
		return text
	}
	half := floorDiv(limit, 2)
	return pystr.SliceTo(text, half) + "\n[... " + strconv.Itoa(n-2*half) + " characters cut ...]\n" +
		pystr.SliceFrom(text, -half) // text[-0:] is all of text, as in Python
}

// floorDiv returns Python's a // b for b > 0.
func floorDiv(a, b int) int {
	if q := a / b; a%b != 0 && a < 0 {
		return q - 1
	} else {
		return q
	}
}

// clipSource clips text, but keeps the first match of the longest query term in the middle when the
// plain head and tail would lose it.
//
// py: agent_decisions/evidence.py _clip_source
func clipSource(text, query string, limit int) string {
	existing := Clip(text, limit)
	n := pystr.Len(text)
	if n <= limit {
		return existing
	}
	terms := slices.Sorted(maps.Keys(Terms(Clip(query, policy.ClipChars))))
	slices.SortStableFunc(terms, func(a, b string) int { return len(b) - len(a) }) // ASCII terms: bytes are code points
	for _, term := range terms[:min(len(terms), maxSourceQueryTerms)] {
		find := termFinder(term)
		match := find(text, 0)
		if match == nil {
			continue
		}
		if find(existing, 0) != nil {
			return existing
		}
		edge := floorDiv(limit, 4)
		width := limit - 2*edge
		start := min(max(pystr.Len(text[:match[0]])-floorDiv(width, 2), edge), n-edge-width)
		var parts strings.Builder
		end := 0
		for _, span := range [][2]int{{0, edge}, {start, start + width}, {n - edge, n}} {
			if span[0] > end {
				fmt.Fprintf(&parts, "\n[... %d characters cut ...]\n", span[0]-end)
			}
			parts.WriteString(pystr.Slice(text, span[0], span[1]))
			end = span[1]
		}
		return parts.String()
	}
	return existing
}

// termFinder returns the finder of re.compile(r"\b" + re.escape(term) + r"\b", re.I) for a term of
// _WORD characters.
func termFinder(term string) finder {
	pattern := "(?i)" + strings.ReplaceAll(term, "i", `[i\x{130}\x{131}]`) // a term needs no escaping
	return wordBounded(regexp.MustCompile(pattern), true)
}

// ShortenObservations returns bundle with its optional history shortened, or nil when nothing can be cut.
// The current executable input is outside this bundle.
//
// py: agent_decisions/evidence.py shorten_observations
func ShortenObservations(bundle *json.Object, query string, contracts []string) (*json.Object, error) {
	result := bundle.Copy()
	cut := 0
	protected := map[string]bool{}
	for _, path := range contracts {
		protected[path] = true
	}
	observations, err := objectValue(bundle, "contract_observations", "keys")
	if err != nil {
		return nil, err
	}
	for _, path := range observations.Keys() {
		protected[path] = true
	}
	for _, step := range [][2]string{{"tool_calls_newest_first", "input"}, {"tool_calls_newest_first", "output"},
		{"source_files", "text"}} {
		field, textKey := step[0], step[1]
		current, err := objectList(result, field)
		if err != nil {
			return nil, err
		}
		items := []any{}
		for _, item := range current {
			text, err := stringValue(item, textKey)
			if err != nil {
				return nil, err
			}
			path, err := stringValue(item, "path")
			if err != nil {
				return nil, err
			}
			if field == "source_files" && (protected[path] || slices.Contains(ruleFiles, pathlib.Name(pathlib.Str(path)))) {
				items = append(items, item)
				continue
			}
			length := pystr.Len(text)
			limit := max(128, min(policy.ClipChars/16, length/16))
			var shorter string
			if textKey == "text" || textKey == "input" {
				shorter = clipSource(text, query, limit)
			} else {
				shorter = Clip(text, limit)
			}
			cut += length - pystr.Len(shorter)
			shortened := item.Copy()
			shortened.Set(textKey, shorter)
			if field == "source_files" && shorter != text {
				shortened.Set("truncated", true)
			}
			if textKey == "input" && shorter != text {
				shortened.Set("input_truncated", true)
				shortened.Set("input_original_chars", json.IntOf(int64(length)))
			}
			items = append(items, shortened)
		}
		if _, ok := bundle.Get(field); ok {
			result.Set(field, items)
		}
	}
	if cut <= 0 {
		return nil, nil
	}
	metadata, err := objectValue(bundle, "metadata", "keys")
	if err != nil {
		return nil, err
	}
	sources, err := objectList(result, "source_files")
	if err != nil {
		return nil, err
	}
	metadata = metadata.Copy()
	metadata.Set("context_limit_chars_cut", json.IntOf(int64(cut)))
	metadata.Set("source_files_truncated", json.IntOf(int64(countTruncated(sources))))
	result.Set("metadata", metadata)
	return result, nil
}

// Options holds the keyword arguments of build.
type Options struct {
	Query     string
	Cwd       *string // nil is None: no source files
	Contracts []string
	Profile   policy.EvidenceProfile // zero preserves the legacy Jev limits
	// Frame is the depth of the Python stack at the build call, counting build: 7 when hook.py's _context
	// calls it (<module>, main, handle, _pre_tool_use, _gate or _decide, _context, build).
	Frame int
}

type indexedText struct {
	idx  int
	text string
}

type indexedCall struct {
	idx  int
	call transcript.ToolCall
}

// Build returns the evidence bundle for t: user messages, tool observations and source files that fit the
// fixed evidence profile, with metadata on what was left out.
//
// py: agent_decisions/evidence.py build
func Build(t transcript.Transcript, secrets []string, o Options) (*json.Object, error) {
	profile, err := normalizeEvidenceProfile(o.Profile)
	if err != nil {
		return nil, err
	}
	clean := func(text string) string {
		return Clip(Redact(text, secrets), profile.TextChars)
	}

	usableCalls, err := usable(t.ToolCalls)
	if err != nil {
		return nil, err
	}
	metadata := json.NewObject(
		"evidence_budget_chars", json.IntOf(int64(profile.BudgetChars)),
		"text_clip_chars", json.IntOf(int64(profile.TextChars)),
		"user_messages_total", json.IntOf(int64(len(t.UserMessages))),
		"user_messages_included", json.IntOf(0),
		"user_messages_omitted", json.IntOf(0),
		"tool_calls_total", json.IntOf(int64(len(usableCalls))),
		"tool_calls_included", json.IntOf(0),
		"tool_calls_omitted", json.IntOf(0),
		"source_files_candidates", json.IntOf(0),
		"source_files_included", json.IntOf(0),
		"source_files_omitted", json.IntOf(0),
		"source_files_too_large", json.IntOf(0),
		"source_files_truncated", json.IntOf(0),
	)
	bundle := json.NewObject("user_messages", []any{}, "tool_calls_newest_first", []any{}, "metadata", metadata)
	usedChars, err := dumpedLen(bundle)
	if err != nil {
		return nil, err
	}

	selectedUsers := selectUserMessages(t.UserMessages, o.Query, clean)
	chosenUsers := map[int]string{}

	includeUser := func(idx int, text string) error {
		trial := map[int]string{idx: text}
		for i, chosen := range chosenUsers {
			if i != idx {
				trial[i] = chosen
			}
		}
		trialMessages := []any{}
		for _, i := range slices.Sorted(maps.Keys(trial)) {
			trialMessages = append(trialMessages, trial[i])
		}
		ok, err := fitsWith(bundle, "user_messages", trialMessages, contextMetadataChars, profile.BudgetChars)
		if err != nil || !ok {
			return err
		}
		chosenUsers[idx] = text
		bundle.Set("user_messages", trialMessages)
		usedChars, err = dumpedLen(bundle)
		return err
	}

	// Keep the original constraint and current instruction ahead of optional history.
	byIndex := slices.Clone(selectedUsers)
	slices.SortFunc(byIndex, func(a, b indexedText) int { return a.idx - b.idx })
	for _, user := range byIndex {
		if user.idx == 0 || user.idx == len(t.UserMessages)-1 {
			if err := includeUser(user.idx, user.text); err != nil {
				return nil, err
			}
		}
	}

	if t.Brief != nil {
		brief := clean(*t.Brief)
		trial := bundle.Copy()
		trial.Set("brief", brief)
		trial.Set("brief_trust", "unverified_parent_agent_brief")
		ok, err := fits(trial, contextMetadataChars, profile.BudgetChars)
		if err != nil {
			return nil, err
		}
		if ok {
			bundle.Set("brief", brief)
			bundle.Set("brief_trust", "unverified_parent_agent_brief")
			if usedChars, err = dumpedLen(bundle); err != nil {
				return nil, err
			}
		} else {
			metadata.Set("brief_omitted", json.IntOf(1))
		}
	}

	selectedCalls := selectToolCalls(usableCalls, o.Query)
	chosenCalls := map[int]*json.Object{}

	includeCall := func(idx int, call transcript.ToolCall) error {
		projected := json.NewObject("id", call.ID, "tool", call.Name, "input", Clip(call.Input, profile.TextChars),
			"output", Clip(call.Output, profile.TextChars), "trust", "unverified_tool_observation")
		if call.Execution != nil {
			execution, err := Scrub(call.Execution, secrets, o.Frame+2) // build, then include_call
			if err != nil {
				return err
			}
			projected.Set("execution", execution)
		}
		remaining := profile.BudgetChars - contextMetadataChars - usedChars
		if pystr.Len(projected.Value("input").(string))+pystr.Len(projected.Value("output").(string)) > remaining {
			return nil // even unescaped text cannot fit, so do not encode the full candidate
		}
		separator := 0
		if len(bundle.Value("tool_calls_newest_first").([]any)) > 0 {
			separator = 2
		}
		length, err := dumpedLen(projected)
		if err != nil || length+separator > remaining {
			return err
		}
		// Redact complete retained texts before clipping, so boundary fragments cannot expose secrets.
		item := projected.Copy()
		item.Set("input", clean(call.Input))
		item.Set("output", clean(call.Output))
		trial := map[int]*json.Object{idx: item}
		for i, chosen := range chosenCalls {
			if i != idx {
				trial[i] = chosen
			}
		}
		trialCalls := []any{}
		for _, i := range slices.Backward(slices.Sorted(maps.Keys(trial))) {
			trialCalls = append(trialCalls, trial[i])
		}
		added, err := dumpedLen(item)
		if err != nil {
			return err
		}
		if added += separator; added <= remaining {
			chosenCalls[idx] = item
			bundle.Set("tool_calls_newest_first", trialCalls)
			usedChars += added
		}
		return nil
	}

	sourceItems, sourceOmitted, sourceTooLarge, scanLimited, err := sourceFilesWithProfile(t, o.Query, o.Cwd,
		secrets, o.Contracts, o.Frame+1, profile)
	if err != nil {
		return nil, err
	}
	metadata.Set("source_files_candidates", json.IntOf(int64(len(sourceItems)+sourceOmitted+sourceTooLarge)))
	if len(sourceItems) > 0 || o.Cwd != nil {
		bundle.Set("source_files", []any{})
	}

	includeSource := func(item *json.Object) error {
		current, _ := bundle.Value("source_files").([]any)
		trialItems := append(slices.Clone(current), item)
		ok, err := fitsWith(bundle, "source_files", trialItems, contextMetadataChars, profile.BudgetChars)
		if err != nil {
			return err
		}
		if !ok {
			sourceOmitted++
			return nil
		}
		bundle.Set("source_files", trialItems)
		usedChars, err = dumpedLen(bundle)
		return err
	}

	// Required contracts must survive optional observations or the preflight
	// asks for a file the hook has already read. Keep each source only once.
	requiredPaths := map[string]bool{}
	for _, path := range o.Contracts {
		requiredPaths[pathlib.Str(path)] = true
	}
	if _, ok := bundle.Get("source_files"); ok {
		required := []any{}
		for _, item := range sourceItems {
			if requiredPaths[item.Value("path").(string)] {
				required = append(required, item)
			}
		}
		bundle.Set("source_files", required)
	}
	if err := TrimToBudgetWithProfile(bundle, o.Contracts, o.Query, contextMetadataChars, profile); err != nil {
		return nil, err
	}
	if usedChars, err = dumpedLen(bundle); err != nil {
		return nil, err
	}
	sourceItems = slices.DeleteFunc(sourceItems, func(item *json.Object) bool {
		return requiredPaths[item.Value("path").(string)]
	})

	// Preserve cited native reads even when their cwd differs from the consultation root.
	requested, err := pathCandidateSet([]string{o.Query}, o.Frame)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for path := range requested {
		names[pathlib.Name(pathlib.Str(path))] = true
	}
	newestFirst := slices.Clone(selectedCalls)
	slices.SortStableFunc(newestFirst, func(a, b indexedCall) int { return b.idx - a.idx })
	for _, selected := range newestFirst {
		cited := false
		for name := range names {
			cited = cited || strings.Contains(selected.call.Input, name)
		}
		if selected.call.Output == "" || !cited {
			continue
		}
		paths, err := pathCandidateSet([]string{selected.call.Input}, o.Frame)
		if err != nil {
			return nil, err
		}
		if hitsAny(requested, paths) {
			if err := includeCall(selected.idx, selected.call); err != nil {
				return nil, err
			}
		}
		if len(chosenCalls) >= maxSourceFiles {
			break
		}
	}

	// Source discovery ranks the requested helper first. Older history must not displace it.
	for _, item := range sourceItems[:min(len(sourceItems), 1)] {
		if err := includeSource(item); err != nil {
			return nil, err
		}
	}
	if usedChars, err = dumpedLen(bundle); err != nil {
		return nil, err
	}
	for _, selected := range newestFirst[:min(len(newestFirst), 2)] {
		if _, ok := chosenCalls[selected.idx]; !ok {
			if err := includeCall(selected.idx, selected.call); err != nil {
				return nil, err
			}
		}
	}
	for _, item := range sourceItems[min(len(sourceItems), 1):] {
		if err := includeSource(item); err != nil {
			return nil, err
		}
	}
	sources, _ := bundle.Value("source_files").([]any)
	metadata.Set("source_files_included", json.IntOf(int64(len(sources))))
	metadata.Set("source_files_omitted", json.IntOf(int64(sourceOmitted)))
	metadata.Set("source_files_too_large", json.IntOf(int64(sourceTooLarge)))
	metadata.Set("source_files_omitted_is_lower_bound", scanLimited)
	metadata.Set("source_files_truncated", json.IntOf(int64(countTruncatedItems(sources))))

	for _, user := range selectedUsers {
		if _, ok := chosenUsers[user.idx]; !ok {
			if err := includeUser(user.idx, user.text); err != nil {
				return nil, err
			}
		}
	}
	userMessages := len(bundle.Value("user_messages").([]any))
	metadata.Set("user_messages_included", json.IntOf(int64(userMessages)))
	metadata.Set("user_messages_omitted", json.IntOf(int64(max(0, len(t.UserMessages)-userMessages))))
	if usedChars, err = dumpedLen(bundle); err != nil {
		return nil, err
	}

	for _, selected := range selectedCalls {
		if _, ok := chosenCalls[selected.idx]; !ok {
			if err := includeCall(selected.idx, selected.call); err != nil {
				return nil, err
			}
		}
	}
	toolCalls := len(bundle.Value("tool_calls_newest_first").([]any))
	metadata.Set("tool_calls_included", json.IntOf(int64(toolCalls)))
	metadata.Set("tool_calls_omitted", json.IntOf(int64(max(0, len(usableCalls)-toolCalls))))
	if err := TrimToBudgetWithProfile(bundle, o.Contracts, o.Query, 0, profile); err != nil {
		return nil, err
	}
	return bundle, nil
}

// usable returns the calls that are not ts-decide calls.
func usable(calls []transcript.ToolCall) ([]transcript.ToolCall, error) {
	var result []transcript.ToolCall
	for _, call := range calls {
		decide, err := transcript.IsDecide(call)
		if err != nil {
			return nil, err
		}
		if !decide {
			result = append(result, call)
		}
	}
	return result, nil
}

// hitsAny reports whether a requested path equals a cited path, or one ends with "/" and the other.
func hitsAny(requested, paths map[string]bool) bool {
	for wanted := range requested {
		for path := range paths {
			if wanted == path || strings.HasSuffix(wanted, "/"+path) || strings.HasSuffix(path, "/"+wanted) {
				return true
			}
		}
	}
	return false
}

// dumpedLen returns len(json.dumps(v, sort_keys=True)).
func dumpedLen(v any) (int, error) {
	text, err := json.Dumps(v, json.Options{SortKeys: true})
	return pystr.Len(text), err
}

// fits reports whether bundle encodes within budgetChars after reservedChars.
//
// py: agent_decisions/evidence.py _fits
func fits(bundle *json.Object, reservedChars, budgetChars int) (bool, error) {
	length, err := dumpedLen(bundle)
	return length+reservedChars <= budgetChars, err
}

// fitsWith returns _fits({**bundle, key: value}, reserved_chars=reservedChars).
func fitsWith(bundle *json.Object, key string, value any, reservedChars, budgetChars int) (bool, error) {
	trial := bundle.Copy()
	trial.Set(key, value)
	return fits(trial, reservedChars, budgetChars)
}

// TrimToBudget applies the legacy Jev evidence limits to bundle's optional parts, in place.
//
// py: agent_decisions/evidence.py _trim_to_budget
func TrimToBudget(bundle *json.Object, contracts []string, query string, reservedChars int) error {
	return TrimToBudgetWithProfile(bundle, contracts, query, reservedChars, policy.EvidenceProfile{})
}

// TrimToBudgetWithProfile drops and shortens bundle's optional parts, in place, until its serialized
// character count fits the fixed profile after reservedChars.
func TrimToBudgetWithProfile(bundle *json.Object, contracts []string, query string, reservedChars int,
	requested policy.EvidenceProfile) error {
	profile, err := normalizeEvidenceProfile(requested)
	if err != nil {
		return err
	}
	return trimToBudget(bundle, contracts, query, reservedChars, profile)
}

func trimToBudget(bundle *json.Object, contracts []string, query string, reservedChars int,
	profile policy.EvidenceProfile) error {
	metadata, err := objectValue(bundle, "metadata", "get") // without one, Python updates a dict that it drops
	if err != nil {
		return err
	}
	required := map[string]bool{}
	for _, path := range contracts {
		required[pathlib.Str(path)] = true
	}
	// over reports whether the bundle does not fit and bundle[key] is truthy.
	over := func(key string) (bool, error) {
		ok, err := fits(bundle, reservedChars, profile.BudgetChars)
		return !ok && json.Truthy(bundle.Value(key)), err
	}
	pop := func(key string) ([]any, error) {
		list, ok := bundle.Value(key).([]any)
		if !ok {
			return nil, &pyerr.Error{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'pop'", json.TypeName(bundle.Value(key)))}
		}
		list = list[:len(list)-1]
		bundle.Set(key, list)
		return list, nil
	}

	for {
		if again, err := over("architecture_history"); err != nil || !again {
			if err != nil {
				return err
			}
			break
		}
		if _, err := pop("architecture_history"); err != nil {
			return err
		}
	}
	for {
		if again, err := over("tool_calls_newest_first"); err != nil || !again {
			if err != nil {
				return err
			}
			break
		}
		calls, err := pop("tool_calls_newest_first")
		if err != nil {
			return err
		}
		metadata.Set("tool_calls_included", json.IntOf(int64(len(calls))))
		if err := increment(metadata, "tool_calls_omitted"); err != nil {
			return err
		}
	}
	for {
		if again, err := over("source_files"); err != nil || !again {
			if err != nil {
				return err
			}
			break
		}
		sources, err := objectList(bundle, "source_files")
		if err != nil {
			return err
		}
		optional := -1
		for i := len(sources) - 1; i >= 0; i-- {
			path, _ := sources[i].Value("path").(string)
			if !required[path] {
				optional = i
				break
			}
		}
		if optional >= 0 {
			bundle.Set("source_files", slices.Delete(slices.Clone(bundle.Value("source_files").([]any)), optional, optional+1))
			if err := increment(metadata, "source_files_omitted"); err != nil {
				return err
			}
			continue
		}
		longest, longestLen := sources[0], -1
		for _, source := range sources {
			text, err := stringValue(source, "text")
			if err != nil {
				return err
			}
			if n := pystr.Len(text); n > longestLen {
				longest, longestLen = source, n
			}
		}
		text, _ := stringValue(longest, "text")
		if text == "" {
			break
		}
		shorter := ""
		if longestLen > 256 {
			shorter = clipSource(text, query, max(128, longestLen/2))
		}
		if pystr.Len(shorter) >= longestLen {
			shorter = ""
		}
		longest.Set("text", shorter)
		longest.Set("truncated", true)
	}
	if _, ok := bundle.Get("source_files"); ok {
		sources, err := objectList(bundle, "source_files")
		if err != nil {
			return err
		}
		metadata.Set("source_files_included", json.IntOf(int64(len(sources))))
		metadata.Set("source_files_truncated", json.IntOf(int64(countTruncated(sources))))
	}
	for {
		ok, err := fits(bundle, reservedChars, profile.BudgetChars)
		if err != nil {
			return err
		}
		messages, _ := bundle.Value("user_messages").([]any)
		if ok || len(messages) <= 1 {
			break
		}
		messages, err = pop("user_messages")
		if err != nil {
			return err
		}
		metadata.Set("user_messages_included", json.IntOf(int64(len(messages))))
		if err := increment(metadata, "user_messages_omitted"); err != nil {
			return err
		}
	}
	for {
		if again, err := over("user_messages"); err != nil || !again {
			if err != nil {
				return err
			}
			break
		}
		messages := bundle.Value("user_messages").([]any)
		text, ok := messages[0].(string)
		if !ok {
			return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("object of type '%s' has no len()", json.TypeName(messages[0]))}
		}
		length := pystr.Len(text)
		shorter := ""
		if length > 256 {
			shorter = Clip(text, max(128, length/2))
		}
		if pystr.Len(shorter) >= length {
			shorter = ""
		}
		messages[0] = shorter
		if shorter == "" {
			if _, err := pop("user_messages"); err != nil {
				return err
			}
			metadata.Set("user_messages_included", json.IntOf(0))
			if err := increment(metadata, "user_messages_omitted"); err != nil {
				return err
			}
		}
	}
	return nil
}

// increment ports metadata[key] = metadata.get(key, 0) + 1.
func increment(metadata *json.Object, key string) error {
	value, ok := metadata.Get(key)
	if !ok {
		metadata.Set(key, json.IntOf(1))
		return nil
	}
	var n big.Int
	switch value := value.(type) {
	case json.Int:
		n.SetString(string(value), 10)
	case bool:
		if value {
			n.SetInt64(1)
		}
	default:
		return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf(
			"unsupported operand type(s) for +: '%s' and 'int'", json.TypeName(value))}
	}
	metadata.Set(key, json.Int(n.Add(&n, big.NewInt(1)).String()))
	return nil
}

// countTruncated returns sum(bool(item.get("truncated")) for item in items).
func countTruncated(items []*json.Object) int {
	count := 0
	for _, item := range items {
		if json.Truthy(item.Value("truncated")) {
			count++
		}
	}
	return count
}

// countTruncatedItems is countTruncated for the source items that build itself made.
func countTruncatedItems(items []any) int {
	count := 0
	for _, item := range items {
		if json.Truthy(item.(*json.Object).Value("truncated")) {
			count++
		}
	}
	return count
}

// Terms returns the set of lower-cased words of three or more ASCII letters, digits or underscores in
// text. gates.py calls this Python-private function.
//
// The matches of _WORD, [A-Za-z0-9_]{3,}, are the runs of three or more ASCII word bytes.
//
// py: agent_decisions/evidence.py _terms
func Terms(text string) map[string]bool {
	terms := map[string]bool{}
	eachTerm(text, func(term string) { terms[term] = true })
	return terms
}

// eachTerm passes each match of _WORD in text to yield, lowercased.
func eachTerm(text string, yield func(term string)) {
	for i := 0; i < len(text); i++ {
		start := i
		for i < len(text) && isASCIIWord(text[i]) {
			i++
		}
		if i-start >= 3 {
			yield(strings.ToLower(text[start:i])) // ASCII
		}
	}
}

func isASCIIWord(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// looksLikeConstraint reports whether text holds a constraint word.
//
// py: agent_decisions/evidence.py _looks_like_constraint
func looksLikeConstraint(text string) bool {
	lower := pystr.Lower(text)
	for _, hint := range constraintHints {
		if strings.Contains(lower, hint) { // ASCII hints
			return true
		}
	}
	return false
}

// termOverlap returns len(queryTerms & Terms(text)) without collecting the terms of text.
func termOverlap(queryTerms map[string]bool, text string) int {
	if len(queryTerms) == 0 {
		return 0
	}
	shared := map[string]bool{}
	eachTerm(text, func(term string) {
		if queryTerms[term] {
			shared[term] = true
		}
	})
	return len(shared)
}

// py: agent_decisions/evidence.py _text_score
func textScore(text string, queryTerms map[string]bool) int {
	score := 100 * termOverlap(queryTerms, text)
	if looksLikeConstraint(text) {
		score += 40
	}
	return score
}

// selectUserMessages ranks messages: the first, then those that share query terms or state a
// constraint, then the most recent, then the rest. Each text comes cleaned.
//
// py: agent_decisions/evidence.py _select_user_messages
func selectUserMessages(messages []string, query string, clean func(string) string) []indexedText {
	type ranked struct {
		group, overlap int
		unconstrained  bool
		idx            int
		text           string
	}
	queryTerms := Terms(query)
	rows := make([]ranked, len(messages))
	for idx, message := range messages {
		shared := termOverlap(queryTerms, message)
		constrained := looksLikeConstraint(message)
		group := 3
		switch {
		case idx == 0:
			group = 0
		case shared > 0 || constrained:
			group = 1
		case idx >= max(0, len(messages)-policy.UserMessages):
			group = 2
		}
		rows[idx] = ranked{group, shared, !constrained, idx, clean(message)}
	}
	slices.SortFunc(rows, func(a, b ranked) int {
		switch {
		case a.group != b.group:
			return a.group - b.group
		case a.overlap != b.overlap:
			return b.overlap - a.overlap
		case a.unconstrained != b.unconstrained:
			if a.unconstrained {
				return 1
			}
			return -1
		}
		return b.idx - a.idx
	})
	result := make([]indexedText, len(rows))
	for i, row := range rows {
		result[i] = indexedText{row.idx, row.text}
	}
	return result
}

// selectToolCalls ranks calls by query relevance, and by recency without a query.
//
// py: agent_decisions/evidence.py _select_tool_calls
func selectToolCalls(calls []transcript.ToolCall, query string) []indexedCall {
	queryTerms := Terms(query)
	scores := make([]int, len(calls))
	result := make([]indexedCall, len(calls))
	for idx, call := range calls {
		searchable := call.Name + "\n" + Clip(call.Input, policy.ClipChars) + "\n" + Clip(call.Output, policy.ClipChars)
		scores[idx] = textScore(searchable, queryTerms)
		if len(queryTerms) == 0 {
			scores[idx] += idx
		}
		result[idx] = indexedCall{idx, call}
	}
	slices.SortFunc(result, func(a, b indexedCall) int {
		if scores[a.idx] != scores[b.idx] {
			return scores[b.idx] - scores[a.idx]
		}
		return b.idx - a.idx
	})
	return result
}

// sourceFiles reads the files that the query, the calls and the user messages name under cwd, with the
// project rule files, and returns them with the counts of omitted and too large candidates and whether the
// candidate scan stopped early. frame is the depth of the Python stack at the _source_files call.
//
// py: agent_decisions/evidence.py _source_files
func sourceFiles(t transcript.Transcript, query string, cwd *string, secrets, contracts []string,
	frame int) (items []*json.Object, omitted, tooLarge int, scanLimited bool, err error) {
	profile, err := normalizeEvidenceProfile(policy.EvidenceProfile{})
	if err != nil {
		return nil, 0, 0, false, err
	}
	return sourceFilesWithProfile(t, query, cwd, secrets, contracts, frame, profile)
}

func sourceFilesWithProfile(t transcript.Transcript, query string, cwd *string, secrets, contracts []string,
	frame int, profile policy.EvidenceProfile) (items []*json.Object, omitted, tooLarge int, scanLimited bool, err error) {
	if cwd == nil {
		return nil, 0, 0, false, nil
	}
	root, err := resolve(*cwd, true)
	if err != nil {
		if isOSError(err) {
			return nil, 0, 0, false, nil
		}
		return nil, 0, 0, false, err
	}

	candidates := &json.Object{} // rel -> path, in insertion order
	requested, err := pathCandidateSet([]string{query}, frame)
	if err != nil {
		return nil, 0, 0, false, err
	}
	requestedPaths := map[string]bool{}
	rankedCandidates, omitted, scanLimited, err := rankedPathCandidates(t, query, frame+1)
	if err != nil {
		return nil, 0, 0, false, err
	}
	if len(contracts) > 0 {
		withContracts := []string{}
		seen := map[string]bool{}
		for _, raw := range slices.Concat(contracts, rankedCandidates) {
			if !seen[raw] {
				seen[raw] = true
				withContracts = append(withContracts, raw)
			}
		}
		extra := max(0, len(withContracts)-maxSourceCandidateProbes)
		omitted += extra
		scanLimited = scanLimited || extra > 0
		rankedCandidates = withContracts[:min(len(withContracts), maxSourceCandidateProbes)]
		for _, path := range contracts {
			requested[path] = true
		}
	}
	for _, raw := range rankedCandidates {
		path, ok, err := resolveSource(root, raw)
		if err != nil {
			return nil, 0, 0, false, err
		}
		if !ok {
			omitted++
			continue
		}
		rel := relativeTo(path, root)
		if _, ok := candidates.Get(rel); !ok {
			candidates.Set(rel, path)
		}
		if requested[raw] {
			requestedPaths[rel] = true
		}
	}
	prioritized := &json.Object{}
	for _, rel := range candidates.Keys() {
		if requestedPaths[rel] {
			prioritized.Set(rel, candidates.Value(rel))
		}
	}
	for _, name := range ruleFiles {
		path := pathlib.Join(root, name)
		if _, ok := prioritized.Get(name); !ok && ospath.IsFile(path) {
			prioritized.Set(name, path)
		}
	}
	for _, rel := range candidates.Keys() {
		prioritized.Set(rel, candidates.Value(rel))
	}

	revision := gitRevision(root)
	for _, rel := range prioritized.Keys() {
		if len(items) >= maxSourceFiles {
			omitted++
			continue
		}
		item, reason, err := readSource(root, rel, prioritized.Value(rel).(string), secrets, query,
			profile.TextChars)
		if err != nil {
			return nil, 0, 0, false, err
		}
		switch {
		case item != nil:
			item.Set("git_baseline", gitBaseline(root, rel, revision, item.Value("sha256").(string)))
			items = append(items, item)
		case reason == "too_large":
			tooLarge++
		default:
			omitted++
		}
	}
	return items, omitted, tooLarge, scanLimited, nil
}

// runGit runs `git -C root args...` as subprocess.run(..., capture_output=True, timeout=1) does, and
// returns its stdout and exit status. err is the OSError or TimeoutExpired that run raises.
func runGit(root string, args ...string) (stdout []byte, status int, err error) {
	git, err := lookPath("git")
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, append([]string{"-C", root}, args...)...)
	cmd.Args[0] = "git"
	cmd.Stdin = os.Stdin // stdin=None inherits it
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.WaitDelay = gitTimeout // a child that keeps the pipes open after git exits holds communicate() too
	err = cmd.Run()
	if err != nil && (ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay)) {
		return nil, 0, context.DeadlineExceeded
	}
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		return out.Bytes(), exit.ExitCode(), nil
	}
	return out.Bytes(), 0, err
}

// lookPath returns the first file named name in os.get_exec_path() that has an execute bit: PATH, or
// os.defpath when PATH is unset. An empty PATH entry is the current directory. Python's subprocess runs
// the first such file; exec.LookPath differs on an unset PATH and on relative entries.
func lookPath(name string) (string, error) {
	path, ok := os.LookupEnv("PATH")
	if !ok {
		path = "/bin:/usr/bin"
	}
	for _, dir := range strings.Split(path, ":") {
		candidate := name
		if dir != "" {
			candidate = strings.TrimSuffix(dir, "/") + "/" + name
		}
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", pyerr.NewOSError(syscall.ENOENT, name)
}

// gitRevision returns the commit that HEAD names in root, or nil.
//
// py: agent_decisions/evidence.py _git_revision
func gitRevision(root string) *string {
	stdout, status, err := runGit(root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil
	}
	value := pystr.Strip(decodeASCII(stdout))
	if status != 0 || !gitRevisionPattern.MatchString(value) {
		return nil
	}
	return &value
}

// decodeASCII returns data.decode("ascii", errors="replace").
func decodeASCII(data []byte) string {
	var out strings.Builder
	for _, b := range data {
		if b < 0x80 {
			out.WriteByte(b)
		} else {
			out.WriteRune(utf8.RuneError)
		}
	}
	return out.String()
}

// gitBaseline returns how rel at revision compares with the worktree file whose sha256 is currentHash.
//
// py: agent_decisions/evidence.py _git_baseline
func gitBaseline(root, rel string, revision *string, currentHash string) *json.Object {
	if revision == nil {
		return json.NewObject("status", "unavailable")
	}
	unavailable := json.NewObject("revision", *revision, "status", "unavailable")
	reference := *revision + ":" + rel
	size, status, err := runGit(root, "cat-file", "-s", reference)
	if err != nil {
		return unavailable
	}
	if status != 0 {
		return json.NewObject("revision", *revision, "status", "not_in_baseline")
	}
	n, ok := pyInt(size)
	if !ok {
		return unavailable
	}
	if n.Cmp(big.NewInt(maxSourceBytes)) > 0 {
		return json.NewObject("revision", *revision, "status", "too_large")
	}
	blob, status, err := runGit(root, "cat-file", "blob", reference)
	if err != nil || status != 0 || len(blob) > maxSourceBytes {
		return unavailable
	}
	digest := sha256Hex(blob)
	return json.NewObject("revision", *revision, "status", "observed", "sha256", digest,
		"differs_from_worktree", digest != currentHash)
}

// pyInt returns int(data) for bytes data: ASCII digits with an optional sign, single underscores between
// digits, and ASCII whitespace around them.
func pyInt(data []byte) (*big.Int, bool) {
	text := strings.Trim(string(data), " \t\n\v\f\r")
	digits := strings.TrimLeft(text, "+-")
	if len(text)-len(digits) > 1 || digits == "" || digits[0] == '_' || digits[len(digits)-1] == '_' ||
		strings.Contains(digits, "__") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(text, "_", ""), 10)
	return n, ok
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// ContractObservations returns the current sha256 of each of the first eight distinct paths under cwd,
// without promoting contract text to user authority.
//
// py: agent_decisions/evidence.py contract_observations
func ContractObservations(paths []string, cwd string, secrets []string) (*json.Object, error) {
	return ContractObservationsWithProfile(paths, cwd, secrets, policy.EvidenceProfile{})
}

// ContractObservationsWithProfile returns current hashes using the fixed evidence profile's source
// excerpt limit, without promoting contract text to user authority.
func ContractObservationsWithProfile(paths []string, cwd string, secrets []string,
	requested policy.EvidenceProfile) (*json.Object, error) {
	profile, err := normalizeEvidenceProfile(requested)
	if err != nil {
		return nil, err
	}
	root, err := resolve(cwd, false)
	if err != nil {
		return nil, err
	}
	current := &json.Object{}
	for _, rel := range paths {
		if _, ok := current.Get(rel); ok {
			continue
		}
		if current.Len() == maxSourceFiles {
			break
		}
		var item *json.Object
		path, ok, err := resolveSource(root, rel)
		if err != nil {
			return nil, err
		}
		if ok {
			if item, _, err = readSource(root, rel, path, secrets, "", profile.TextChars); err != nil {
				return nil, err
			}
		}
		if item != nil {
			current.Set(rel, json.NewObject("sha256", item.Value("sha256"), "trust", "verified_file_observation"))
		} else {
			current.Set(rel, json.NewObject("sha256", nil, "trust", "unavailable"))
		}
	}
	return current, nil
}

// ArchitectureHistory returns records newest first, each with the status of its contracts against
// current. Historical decisions never grant authority. Unchecked contracts stay explicit.
//
// py: agent_decisions/evidence.py architecture_history
func ArchitectureHistory(records []*json.Object, current *json.Object) ([]any, error) {
	result := []any{}
	for _, record := range slices.Backward(records) {
		contracts, err := objectValue(record, "contracts", "items")
		if err != nil {
			return nil, err
		}
		statuses := &json.Object{}
		for _, rel := range contracts.Keys() {
			observed, err := objectValue(current, rel, "get")
			if err != nil {
				return nil, err
			}
			_, checked := current.Get(rel)
			actual := observed.Value("sha256")
			status := "changed"
			switch equal, err := json.Equal(actual, contracts.Value(rel)); {
			case !checked:
				status = "not_checked"
			case actual == nil:
				status = "unavailable"
			case err != nil:
				return nil, err
			case equal:
				status = "unchanged"
			}
			statuses.Set(rel, status)
		}
		item := record.Copy()
		item.Set("contract_status", statuses)
		item.Set("trust", "signed_historical_decision")
		item.Set("authorizes_write", false)
		result = append(result, item)
	}
	return result, nil
}

// rankedPathCandidates returns up to sixteen path candidates, best first: the query, then current-turn
// inputs, user messages newest first, older inputs, current-turn outputs and older outputs. omitted is 1
// and scanLimited true when a seventeenth distinct candidate exists. frame is the depth of the Python
// stack at the _ranked_path_candidates call.
//
// py: agent_decisions/evidence.py _ranked_path_candidates
func rankedPathCandidates(t transcript.Transcript, query string, frame int) (ranked []string, omitted int,
	scanLimited bool, err error) {
	turnIDs := map[string]bool{}
	for _, call := range t.TurnCalls {
		turnIDs[call.ID] = true
	}
	terms := Terms(query)
	type scored struct {
		score, index int
		call         transcript.ToolCall
	}
	var calls []scored
	for index, call := range t.ToolCalls {
		decide, err := transcript.IsDecide(call)
		if err != nil {
			return nil, 0, false, err
		}
		if !decide {
			calls = append(calls, scored{textScore(Clip(call.Input, policy.ClipChars), terms), index, call})
		}
	}
	slices.SortFunc(calls, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return b.index - a.index
	})
	var currentInputs, olderInputs, currentOutputs, olderOutputs []string
	for _, c := range calls {
		if turnIDs[c.call.ID] {
			currentInputs, currentOutputs = append(currentInputs, c.call.Input), append(currentOutputs, c.call.Output)
		} else {
			olderInputs, olderOutputs = append(olderInputs, c.call.Input), append(olderOutputs, c.call.Output)
		}
	}
	newestUsers := slices.Clone(t.UserMessages)
	slices.Reverse(newestUsers)
	buckets := [][]string{{query}, currentInputs, newestUsers, olderInputs, currentOutputs, olderOutputs}
	seen := map[string]bool{}
	for _, texts := range buckets {
		full := false
		err := pathCandidates(texts, frame+1, func(raw string) bool {
			if seen[raw] {
				return true
			}
			if len(ranked) == maxSourceCandidateProbes {
				full = true
				return false
			}
			seen[raw] = true
			ranked = append(ranked, raw)
			return true
		})
		if err != nil {
			return nil, 0, false, err
		}
		if full {
			return ranked, 1, true, nil
		}
	}
	return ranked, 0, false, nil
}

// pathCandidateSet returns set(_path_candidates(texts)). frame is the depth of the Python stack at the
// call that builds the set.
func pathCandidateSet(texts []string, frame int) (map[string]bool, error) {
	set := map[string]bool{}
	err := pathCandidates(texts, frame+1, func(raw string) bool {
		set[raw] = true
		return true
	})
	return set, err
}

// pathCandidates passes each distinct path-like string in texts to yield, in order, until yield returns
// false: strings in the JSON of each text, shell-quoted paths with spaces, path tokens, and whitespace
// separated words. Python's generator is lazy, so a later text is not parsed after yield stops. frame is
// the depth of the Python stack at the generator, which is the depth of the Python code that iterates it
// plus one.
//
// py: agent_decisions/evidence.py _path_candidates
func pathCandidates(texts []string, frame int, yield func(string) bool) error {
	seen := map[string]bool{}
	emit := func(cleaned string) bool {
		seen[cleaned] = true
		return yield(cleaned)
	}
	for _, text := range texts {
		text = Clip(text, policy.ClipChars)
		structured, err := json.Loads(text)
		if err != nil {
			if isRecursionError(err) {
				return err
			}
			structured = nil
		}
		more, err := walkStrings(structured, frame+1, func(value string) bool {
			cleaned := cleanPathCandidate(value)
			if !strings.Contains(cleaned, "\n") && pystr.Len(cleaned) <= maxPathCandidateChars &&
				looksPathLike(cleaned) && !seen[cleaned] {
				return emit(cleaned)
			}
			return true
		})
		if err != nil || !more {
			return err
		}
		more, err = stringValues(text, frame+1, func(part string) bool {
			quotedTokens, err := shlex.Split(part)
			if err != nil {
				quotedTokens = nil
			}
			for _, token := range quotedTokens {
				cleaned := cleanPathCandidate(token)
				if strings.Contains(cleaned, " ") && !strings.Contains(cleaned, "\n") && looksPathLike(cleaned) &&
					!seen[cleaned] && !emit(cleaned) {
					return false
				}
			}
			for _, candidate := range pathTokens(part) {
				cleaned := cleanPathCandidate(candidate)
				if cleaned != "" && !seen[cleaned] && !emit(cleaned) {
					return false
				}
			}
			// re.split(r"\s+", part) also gives empty strings at the ends, which clean to "".
			for _, token := range pystr.Split(part) {
				cleaned := cleanPathCandidate(token)
				if cleaned != "" && !seen[cleaned] && looksPathLike(cleaned) && !emit(cleaned) {
					return false
				}
			}
			return true
		})
		if err != nil || !more {
			return err
		}
	}
	return nil
}

// pathTokens returns _PATH_TOKEN.findall(text). The prefix (?:^|[...]) can only start a match at the
// start of text or at a prefix character, and the groups cannot hold one, so each attempt runs anchored
// and the scan stays linear.
//
// py: agent_decisions/evidence.py _PATH_TOKEN
func pathTokens(text string) []string {
	var found []string
	attempt := func(at int) int {
		if !mayStartPathToken(text[at:]) {
			return -1
		}
		if loc := pathToken.FindStringSubmatchIndex(text[at:]); loc != nil {
			found = append(found, text[at+loc[2]:at+loc[3]])
			return at + loc[3] // the lookahead does not consume
		}
		return -1
	}
	for p := 0; p < len(text); {
		if p == 0 {
			if end := attempt(0); end >= 0 {
				p = end
				continue
			}
		}
		if start := pathTokenStartLen(text[p:]); start > 0 {
			if end := attempt(p + start); end >= 0 {
				p = end
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(text[p:])
		p += size
	}
	return found
}

// pathTokenStartLen returns the length of the pathTokenStart match at the start of a non-empty s, or 0.
func pathTokenStartLen(s string) int {
	if s[0] < utf8.RuneSelf {
		if asciiPathTokenStart[s[0]] {
			return 1
		}
		return 0
	}
	if loc := pathTokenStart.FindStringIndex(s); loc != nil {
		return loc[1]
	}
	return 0
}

// mayStartPathToken reports whether pathToken can match at the start of s. Each alternative of its group
// is a run of [A-Za-z0-9_@+./-] that holds a "/" or a ".", so it cannot match where the run at the start
// of s holds neither. The check skips the regexp at most word starts.
func mayStartPathToken(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '/' || c == '.':
			return true
		case !isASCIIWord(c) && c != '@' && c != '+' && c != '-':
			return false
		}
	}
	return false
}

// stringValues passes text to yield, then every string in its JSON value, and returns false when yield
// stopped. frame is the depth of the _string_values generator.
//
// py: agent_decisions/evidence.py _string_values
func stringValues(text string, frame int, yield func(string) bool) (bool, error) {
	if !yield(text) {
		return false, nil
	}
	value, err := json.Loads(text)
	if err != nil {
		if isRecursionError(err) {
			return false, err
		}
		return true, nil
	}
	return walkStrings(value, frame+1, yield)
}

// walkStrings passes every string in value to yield, depth first, and returns false when yield stopped.
// frame is the depth of the _walk_strings generator for value.
//
// py: agent_decisions/evidence.py _walk_strings
func walkStrings(value any, frame int, yield func(string) bool) (bool, error) {
	if frame > recursionLimit {
		return false, recursionError()
	}
	switch value := value.(type) {
	case string:
		return yield(value), nil
	case []any:
		for _, item := range value {
			if more, err := walkStrings(item, frame+1, yield); err != nil || !more {
				return more, err
			}
		}
	case *json.Object:
		for _, key := range value.Keys() {
			if more, err := walkStrings(value.Value(key), frame+1, yield); err != nil || !more {
				return more, err
			}
		}
	}
	return true, nil
}

func isRecursionError(err error) bool {
	var recursion *json.RecursionError
	return errors.As(err, &recursion)
}

func isOSError(err error) bool {
	var osError *pyerr.OSError
	return errors.As(err, &osError)
}

// cleanPathCandidate strips quotes, brackets, trailing dots and a :line[:column] suffix from raw, and
// returns "" for a URL or an empty, "." or ".." path.
//
// py: agent_decisions/evidence.py _clean_path_candidate
func cleanPathCandidate(raw string) string {
	value := pystr.RStripChars(pystr.StripChars(pystr.Strip(raw), "\"'`“”‘’()[]{}<>,;"), ".")
	// Python's $ also matches before a final "\n", so the suffix may sit there.
	body, newline := value, ""
	if strings.HasSuffix(value, "\n") {
		body, newline = value[:len(value)-1], "\n"
	}
	if strings.IndexByte(body, ':') >= 0 { // the only way lineSuffix can match
		if loc := lineSuffix.FindStringIndex(body); loc != nil {
			value = body[:loc[0]] + newline
		}
	}
	if strings.Contains(value, "://") || value == "" || value == "." || value == ".." {
		return ""
	}
	return value
}

// py: agent_decisions/evidence.py _looks_path_like
func looksPathLike(value string) bool {
	name := pathlib.Name(pathlib.Str(value))
	return strings.Contains(value, "/") || name == ".env" || strings.HasPrefix(name, ".env.") ||
		strings.Contains(name, ".")
}

// resolveSource returns the resolved path of raw under root when it is a readable, non-private regular
// file there. err is a Python exception other than OSError, which Python does not catch here. Tests
// replace it, as the Python tests patch _resolve_source.
//
// py: agent_decisions/evidence.py _resolve_source
var resolveSource = func(root, raw string) (path string, ok bool, err error) {
	path = pathlib.Str(raw)
	if !strings.HasPrefix(path, "/") {
		path = join(root, path)
	}
	resolved, err := resolve(path, true)
	if err != nil {
		if isOSError(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !isRelativeTo(resolved, root) || !ospath.IsFile(resolved) {
		return "", false, nil
	}
	private, err := isPrivate(relativeTo(resolved, root), resolved)
	if err != nil || private {
		return "", false, err
	}
	return resolved, true, nil
}

// join returns str(Path(base) / rel) for a normalized absolute base and a normalized relative rel.
func join(base, rel string) string {
	if rel == "." {
		return base
	}
	return pathlib.Join(base, rel)
}

// isRelativeTo returns Path(path).is_relative_to(root) for two absolute paths that realpath returned.
//
// py: agent_decisions/evidence.py _is_relative_to
func isRelativeTo(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

// relativeTo returns path.relative_to(root).as_posix() for a path that isRelativeTo root.
func relativeTo(path, root string) string {
	if path == root {
		return "."
	}
	return path[len(strings.TrimSuffix(root, "/"))+1:]
}

// isPrivate reports whether rel (resolved, under the source root) names a secret file, a version control
// or key directory, or the agent-decisions state directory. err is the RuntimeError of state_dir().
//
// py: agent_decisions/evidence.py _is_private
func isPrivate(rel, resolved string) (bool, error) {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	if name == "." {
		parts, name = nil, ""
	}
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return true, nil
	}
	if privateNames[name] || slices.ContainsFunc(privateSuffixes, func(suffix string) bool {
		return strings.HasSuffix(name, suffix)
	}) {
		return true, nil
	}
	stateDir, err := policy.StateDir()
	if err == nil {
		var privateState string
		if privateState, err = resolve(stateDir, false); err == nil && isRelativeTo(resolved, privateState) {
			return true, nil
		}
	}
	if err != nil && !isOSError(err) {
		return false, err
	}
	return slices.ContainsFunc(parts, func(part string) bool { return privateParts[part] }), nil
}

// sourceFile is the open source file that readSource reads: *os.File, or a test double. Stat is
// os.fstat(f.fileno()).
type sourceFile interface {
	io.ReadCloser
	Stat() (fs.FileInfo, error)
}

// openSourceFile opens path, under root, without following a symlink in any part below root. Tests
// replace it, as the Python tests patch _open_source_file.
//
// py: agent_decisions/evidence.py _open_source_file
var openSourceFile = func(path, root string) (sourceFile, error) {
	if !isRelativeTo(path, root) {
		return nil, &pyerr.Error{Class: "ValueError", Msg: fmt.Sprintf("%s is not in the subpath of %s",
			pystr.Repr(path), pystr.Repr(root))}
	}
	rel := relativeTo(path, root)
	parts := strings.Split(rel, "/")
	if rel == "." || slices.ContainsFunc(parts, func(part string) bool { return part == "" || part == "." || part == ".." }) {
		return nil, &pyerr.OSError{Class: "OSError", Msg: "invalid relative source path"}
	}
	return openBeneath(root, parts)
}

// sameSourceObservation reports whether two stats show the same file with the same size and mtime.
//
// py: agent_decisions/evidence.py _same_source_observation
func sameSourceObservation(first, second fs.FileInfo) bool {
	a, okA := first.Sys().(*syscall.Stat_t)
	b, okB := second.Sys().(*syscall.Stat_t)
	return okA && okB && a.Dev == b.Dev && a.Ino == b.Ino && first.Size() == second.Size() &&
		first.ModTime().Equal(second.ModTime())
}

// readSource reads path, which resolveSource accepted under root, as an evidence item labelled rel. It
// returns a nil item and the reason ("unreadable", "too_large" or "binary") when the file cannot be used.
// err is a Python exception other than OSError.
//
// py: agent_decisions/evidence.py _read_source
func readSource(root, rel, path string, secrets []string, query string, textChars int) (*json.Object, string, error) {
	initial, err := os.Stat(path)
	if err != nil {
		return nil, "unreadable", nil
	}
	if initial.Size() > maxSourceBytes {
		return nil, "too_large", nil
	}
	data, reason, err := readOpened(root, path, initial)
	if err != nil {
		if isOSError(err) {
			return nil, "unreadable", nil
		}
		return nil, "", err
	}
	if reason != "" {
		return nil, reason, nil
	}
	if len(data) > maxSourceBytes {
		return nil, "too_large", nil
	}
	if bytes.IndexByte(data[:min(len(data), 1024)], 0) >= 0 {
		return nil, "binary", nil
	}
	text := Redact(json.DecodeUTF8(data), secrets)
	truncated := pystr.Len(text) > textChars
	text = clipSource(text, query, textChars)
	return json.NewObject(
		"path", rel,
		"sha256", sha256Hex(data),
		"text", text,
		"source", "hook_file_read",
		"role", "working_tree_source",
		"trust", "verified_file_observation",
		"truncated", truncated,
	), "", nil
}

// readOpened is the `with _open_source_file(...)` block of _read_source: the bytes read, or a reason the
// file cannot be used.
func readOpened(root, path string, initial fs.FileInfo) (data []byte, reason string, err error) {
	f, err := openSourceFile(path, root)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		opened = nil // a file without a stable descriptor
	}
	if opened != nil {
		if !sameSourceObservation(initial, opened) {
			return nil, "unreadable", nil
		}
		if opened.Size() > maxSourceBytes {
			return nil, "too_large", nil
		}
	}
	final, err := resolve(path, true)
	if err != nil {
		if isOSError(err) {
			return nil, "unreadable", nil
		}
		return nil, "", err
	}
	if !isRelativeTo(final, root) {
		return nil, "unreadable", nil
	}
	if private, err := isPrivate(relativeTo(final, root), final); err != nil || private {
		return nil, "unreadable", err
	}
	if data, err = io.ReadAll(io.LimitReader(f, maxSourceBytes+1)); err != nil {
		return nil, "", pyerr.NewOSError(err)
	}
	if opened != nil {
		after, err := f.Stat()
		if err != nil || !sameSourceObservation(opened, after) {
			return nil, "unreadable", nil
		}
	}
	return data, "", nil
}

// resolve returns str(Path(p).resolve(strict=strict)).
func resolve(p string, strict bool) (string, error) {
	return ospath.Realpath(pathlib.Str(p), strict)
}

// objectValue returns bundle.get(key, {}) as a dict, or the AttributeError of the dict method attr that
// Python calls on another value.
func objectValue(bundle *json.Object, key, attr string) (*json.Object, error) {
	value, ok := bundle.Get(key)
	if !ok {
		return &json.Object{}, nil
	}
	object, ok := value.(*json.Object)
	if !ok {
		return nil, &pyerr.Error{Class: "AttributeError",
			Msg: fmt.Sprintf("'%s' object has no attribute '%s'", json.TypeName(value), attr)}
	}
	return object, nil
}

// objectList returns bundle.get(key, ()) as a list of dicts.
func objectList(bundle *json.Object, key string) ([]*json.Object, error) {
	value, ok := bundle.Get(key)
	if !ok {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", json.TypeName(value))}
	}
	objects := make([]*json.Object, len(list))
	for i, item := range list {
		if objects[i], ok = item.(*json.Object); !ok {
			return nil, &pyerr.Error{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", json.TypeName(item))}
		}
	}
	return objects, nil
}

// stringValue returns item.get(key, "") as a str.
func stringValue(item *json.Object, key string) (string, error) {
	value, ok := item.Get(key)
	if !ok {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("object of type '%s' has no len()", json.TypeName(value))}
	}
	return text, nil
}
