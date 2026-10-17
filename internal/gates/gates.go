// Package gates ports agent_decisions/gates.py. Each gate takes a Context and returns an Outcome; the hook
// turns that into host output.
//
// Gates that Python implements with recursive functions take frame, the depth of the Python stack at the
// gate's own frame (with <module> as 1), so that too deep input raises RecursionError where Python does.
package gates

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/provenance"
	"github.com/rexbrahh/make-better-decisions/internal/py/fnmatch"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	pyre "github.com/rexbrahh/make-better-decisions/internal/py/re"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// Tests patch these as the Python tests patch policy.MODEL, policy.REVIEW_CONTRACT, time and uuid.
var (
	model                    = policy.Model
	reviewContract           = policy.ReviewContract
	now                      = time.Now
	random         io.Reader = rand.Reader
)

// Float copies of the thresholds, so that arithmetic on them rounds as Python's floats do.
var (
	bindAt   float64 = policy.BindAt
	stopAt   float64 = policy.StopAt
	followAt float64 = policy.FollowAt
	refuseAt float64 = policy.RefuseAt
	unbound          = round8(1.0 - bindAt) // round(1.0 - BIND_AT, 8)
)

// round8 returns round(x, 8), which rounds the exact binary value to eight decimals, as strconv does.
func round8(x float64) float64 {
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 8, 64), 64)
	return rounded
}

// Ask is the review call: it returns Jev's answers to questions about state.
//
// py: agent_decisions/gates.py Ask
type Ask func(state, questions *json.Object) (*json.Object, error)

// NoneFit is the choice that no listed option fits.
//
// py: agent_decisions/gates.py NONE_FIT
const NoneFit = "none_fit"

// Usage is the ts-decide request syntax.
//
// py: agent_decisions/gates.py USAGE
const Usage = `usage: ts-decide '<json>'   or   ts-decide <<'EOF' ... EOF
json: {"decision": "<question>",
       "options": [{"id": "<a-z0-9_->", "description": "<neutral one line>"}, ...2 or more],
       "facts": ["<verbatim excerpt from a user message>", ...]}
See the ` + "`deciding`" + ` skill.`

// Context is what a gate knows about the call.
//
// py: agent_decisions/gates.py Context
type Context struct {
	Host             string
	SessionID        string
	AgentID          any // the payload's agent_id; nil is None
	ToolUseID        any
	Transcript       transcript.Transcript
	Evidence         *json.Object
	Entries          []*json.Object // this session's ledger lines
	RepoID           string
	InheritedEntries []*json.Object // verified by the host-context loader
	Metrics          *json.Object   // instrumentation never enters model state
	Reviewer         policy.ReviewProvider
}

// Outcome is a gate's result.
//
// py: agent_decisions/gates.py Outcome
type Outcome struct {
	Action  string
	Message string
	Record  *json.Object // gate-specific ledger fields; nil writes no ledger line
	Note    string       // shown to the user as a systemMessage
}

func init() { pyerr.Register("RequestError", "ValueError") }

// requestError is gates.RequestError, a ValueError subclass.
//
// py: agent_decisions/gates.py RequestError
func requestError(format string, args ...any) error {
	return &pyerr.Error{Class: "RequestError", Msg: fmt.Sprintf(format, args...), Module: "agent_decisions.gates"}
}

// py: agent_decisions/gates.py _noul
func noul(instructions, yes, no string) *json.Object {
	return json.NewObject("type", "noul", "instructions", instructions,
		"criteria", json.NewObject("true", yes, "false", no))
}

// py: agent_decisions/gates.py _owner_question
func ownerQuestion(subject string) *json.Object {
	return json.NewObject("type", "choice", "instructions", "Under `policy`, who should decide "+subject+"?",
		"criteria", json.NewObject(
			"agent", "The agent: an engineering detail, or something the user's words or the evidence already settle.",
			"user", "The user: product behaviour, or anything destructive, irreversible, external, or paid, "+
				"that the user's words and the evidence do not settle."))
}

// refused and gather_evidence call again.
var consultedActions = map[string]bool{"binding": true, "advisory": true, "ask_user": true, "unverified": true}

var headings = map[string]string{"binding": "BINDING", "advisory": "ADVISORY", "gather_evidence": "GATHER EVIDENCE",
	"ask_user": "ASK USER", "refused": "REFUSED", "routine": "PROCEED", "parent_review": "PARENT REVIEW"}

var headingActions = func() map[string]bool {
	actions := map[string]bool{}
	for action := range headings {
		actions[action] = true
	}
	return actions
}()

// is reports v == s for a decoded value v.
func is(v any, s string) bool {
	text, ok := v.(string)
	return ok && text == s
}

// Consulted returns the completed verdicts for this agent; each gate checks relevance to the current context.
//
// py: agent_decisions/gates.py consulted
func Consulted(ctx Context) ([]*json.Object, error) {
	var ids []string // calls keeps the first position of an id and the last call with it
	calls := map[string]transcript.ToolCall{}
	for _, call := range ctx.Transcript.ToolCalls {
		decide, err := transcript.IsDecide(call)
		if err != nil {
			return nil, err
		}
		if !decide {
			continue
		}
		if _, ok := calls[call.ID]; !ok {
			ids = append(ids, call.ID)
		}
		calls[call.ID] = call
	}
	var backed []*json.Object
	var e expr
	for _, entry := range ctx.Entries {
		if !matchesReviewIdentity(entry, ctx) {
			continue
		}
		action := entry.Value("action")
		skip := !is(entry.Value("host"), ctx.Host) || !e.equal(entry.Value("agent_id"), ctx.AgentID) ||
			!is(entry.Value("gate"), "decide") || !e.in(action, consultedActions)
		if e.err != nil {
			return nil, e.err
		}
		if skip {
			continue
		}
		expected := "ts-decide verdict: " + headings[action.(string)]
		if action == "unverified" {
			expected = "ts-decide: decision unverified ("
		}
		rendered, ok := entry.Value("verdict").(string)
		if !ok || rendered == "" {
			continue
		}
		var candidates []*transcript.ToolCall
		if receiptID := entry.Value("receipt_id"); receiptID != nil {
			text, ok := receiptID.(string)
			if !ok || !contains(pystr.SplitLines(rendered), "receipt: "+text) {
				continue
			}
			for _, id := range ids {
				call := calls[id]
				candidates = append(candidates, &call)
			}
		} else {
			id, found := e.key(entry.Value("tool_use_id"), "dict key")
			if e.err != nil {
				return nil, e.err
			}
			var call *transcript.ToolCall
			if found {
				if match, ok := calls[id]; ok {
					call = &match
				}
			}
			candidates = append(candidates, call)
		}
		verdict := strings.ReplaceAll(rendered, "\r\n", "\n")
		for _, call := range candidates {
			if call != nil && strings.Contains(call.Output, expected) &&
				strings.Contains(strings.ReplaceAll(call.Output, "\r\n", "\n"), verdict) {
				backed = append(backed, entry)
				break
			}
		}
	}
	for _, entry := range ctx.InheritedEntries {
		if matchesReviewIdentity(entry, ctx) {
			backed = append(backed, entry)
		}
	}
	return backed, nil
}

func contains(items []string, item string) bool {
	for _, candidate := range items {
		if candidate == item {
			return true
		}
	}
	return false
}

// RequestScope returns the validated scope of a ts-decide request.
//
// py: agent_decisions/gates.py _request_scope
func RequestScope(raw string) (*json.Object, error) {
	request, err := json.Loads(raw)
	if err != nil {
		return nil, err
	}
	var e expr
	value := e.get(request, "scope", &json.Object{})
	if e.err != nil {
		return nil, e.err
	}
	scope, ok := value.(*json.Object)
	if ok {
		for _, key := range scope.Keys() {
			if key != "paths" && key != "invariants" {
				ok = false
			}
		}
	}
	if !ok {
		return nil, requestError("scope must contain paths and invariants only")
	}
	for _, field := range []string{"paths", "invariants"} {
		values, ok := e.get(scope, field, []any{}).([]any)
		valid := ok && len(values) <= 32
		for _, value := range values {
			text, ok := value.(string)
			valid = valid && ok && text != "" && pystr.Len(text) <= 500
		}
		if !valid {
			return nil, requestError("scope.%s must contain at most 32 non-empty strings", field)
		}
	}
	for _, value := range e.get(scope, "paths", []any{}).([]any) {
		path := value.(string)
		if isAbs(path) || contains(parts(path), "..") || strings.ContainsAny(path, "*?[]\\") ||
			path == "." || path == "./" {
			return nil, requestError("scope paths must be repository-relative files or directory prefixes, without globs")
		}
	}
	return scope, nil
}

// isOptionID reports _OPTION_ID.fullmatch(id): [a-z0-9_-]{1,40}.
func isOptionID(id string) bool {
	if id == "" || len(id) > 40 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ParseRequest validates a ts-decide request. options maps each option id to its description, in request
// order.
//
// py: agent_decisions/gates.py parse_request
func ParseRequest(raw string) (decision string, options *json.Object, facts []string, err error) {
	if pystr.Len(raw) > policy.RequestChars {
		return "", nil, nil, requestError(
			"request exceeds 16,000 characters; remove padding and keep the choice concise")
	}
	value, err := json.Loads(raw)
	if err != nil {
		if pyerr.IsInstance(err, "ValueError") {
			return "", nil, nil, requestError("not JSON (%s)", err)
		}
		return "", nil, nil, err
	}
	request, ok := value.(*json.Object)
	if !ok {
		return "", nil, nil, requestError("the request must be a JSON object")
	}
	decision, ok = request.Value("decision").(string)
	if !ok || pystr.Strip(decision) == "" {
		return "", nil, nil, requestError("`decision` must be a non-empty string")
	}
	list, ok := request.Value("options").([]any)
	if !ok || len(list) < 2 || len(list) > 254 {
		return "", nil, nil, requestError("`options` must list 2 to 254 options")
	}
	options = &json.Object{}
	for _, item := range list {
		option, ok := item.(*json.Object)
		var id, description string
		if ok {
			id, ok = option.Value("id").(string)
		}
		if ok {
			description, ok = option.Value("description").(string)
		}
		if !ok || pystr.Strip(description) == "" {
			return "", nil, nil, requestError("each option needs a string `id` and a non-empty `description`")
		}
		if _, seen := options.Get(id); !isOptionID(id) || id == NoneFit || seen {
			return "", nil, nil, requestError("option id %s must match [a-z0-9_-]{1,40}, be unique, and not be `%s`",
				pystr.Repr(id), NoneFit)
		}
		options.Set(id, description)
	}
	value, present := request.Get("facts")
	if !present {
		value = []any{}
	}
	items, ok := value.([]any)
	for _, item := range items {
		fact, isText := item.(string)
		ok = ok && isText
		facts = append(facts, fact)
	}
	if !ok {
		return "", nil, nil, requestError("`facts` must be a list of strings")
	}
	return pystr.Strip(decision), options, facts, nil
}

// splitStatements returns re.split(r"(?<=[.!?;])\s+|\n+", text).
//
// py: agent_decisions/gates.py _missing_fact_candidates (hand-port: lookbehind)
func splitStatements(text string) []string {
	var pieces []string
	last := 0
	for i := 0; i < len(text); {
		r, size := pystr.DecodeRune(text[i:])
		end := i
		if i > 0 && strings.IndexByte(".!?;", text[i-1]) >= 0 && pystr.IsSpace(r) {
			for end < len(text) {
				r, size := pystr.DecodeRune(text[end:])
				if !pystr.IsSpace(r) {
					break
				}
				end += size
			}
		} else if r == '\n' {
			for end < len(text) && text[end] == '\n' {
				end++
			}
		}
		if end == i {
			i += size
			continue
		}
		pieces = append(pieces, text[last:i])
		last, i = end, end
	}
	return append(pieces, text[last:])
}

var referringPrefixes = []string{"It ", "Its ", "They ", "Their ", "This ", "These ", "That ", "Those "}

// py: agent_decisions/gates.py _missing_fact_candidates
func missingFactCandidates(decision string, options, scope *json.Object) *json.Object {
	texts := []string{decision}
	for _, id := range options.Keys() {
		texts = append(texts, options.Value(id).(string))
	}
	if invariants, ok := scope.Get("invariants"); ok {
		for _, invariant := range invariants.([]any) {
			texts = append(texts, invariant.(string))
		}
	}
	var statements []string
	seen := map[string]bool{}
	for _, text := range texts {
		previous := ""
		for _, part := range splitStatements(text) {
			part = pystr.Strip(part)
			if part == "" || strings.HasSuffix(part, "?") {
				continue
			}
			// Keep the named subject when a statement refers to the preceding sentence.
			statement := part
			if previous != "" && hasAnyPrefix(part, referringPrefixes...) {
				statement = previous + " " + part
			}
			if !seen[statement] {
				seen[statement] = true
				statements = append(statements, statement)
			}
			previous = statement
		}
	}
	candidates := &json.Object{}
	for index, statement := range statements {
		if index == 32 {
			break
		}
		candidates.Set("claim_"+strconv.Itoa(index), evidence.Clip(statement, 1_000))
	}
	return candidates
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// quote returns json.dumps(text).
func quote(text string) string {
	quoted, _ := json.Dumps(text, json.Options{}) // a str cannot fail
	return quoted
}

// py: agent_decisions/gates.py _decide_questions
func decideQuestions(options, scope, missingFacts *json.Object) *json.Object {
	pick := options.Copy()
	pick.Set(NoneFit, "None of the listed options fits the evidence.")
	argued := json.NewObject("decision", "The decision question")
	for _, id := range options.Keys() {
		argued.Set("option_"+id, "The description of option "+id)
	}
	questions := json.NewObject(
		"pick", json.NewObject("type", "choice",
			"instructions", "Using `evidence`, `facts` and `policy`, which option in `options` best serves "+
				"the user's stated goals for `decision`? `unverified_claims` are assertions by "+
				"the agent that were not found in the transcript; do not treat them as evidence.",
			"criteria", pick),
		"sufficient", noul(
			"Do the requirements, observed behavior or implementation in `evidence` support comparing "+
				"the options for `decision`? Several engineering options can satisfy the same requirement. "+
				"The user need not prescribe a preference between them.",
			"Known requirements and observations provide a basis for comparing the options.",
			"A necessary fact for comparing the options is missing."),
		"pattern_present", noul(
			"Does current source in `evidence` show an interface or data-flow pattern applicable to `decision`?",
			"Observed implementation shows an existing pattern for this kind of operation.",
			"No applicable implementation pattern was observed. A proposal or an assertion about a design is not an implementation."),
		"unsupported", noul(
			"Does choosing the proposed operation require a current feasibility fact that is missing from `evidence`? "+
				"Judge the stage requested by `decision`. A new implementation or a future execution check need not "+
				"be completed to select its preparation approach. Do not treat a proposed check as completed proof. "+
				"A required existing helper or toolchain must still be observed when selection depends on its behavior.",
			"Selection depends on a necessary current fact that has not been observed.",
			"Current observations support selecting the operation. Remaining implementation and execution checks "+
				"are future work, not claimed results."),
		"omitted_requirement", noul(
			"Does `decision` refer to a required constraint whose value is absent from `evidence`?",
			"A referenced constraint is unstated, such as the deployment platform or required compatibility.",
			"The values of the constraints referenced by this decision are present."),
		"missing_option", noul(
			"Does `evidence` name an applicable alternative for `decision` that is absent from `options`?",
			"The user or observed implementation names an applicable alternative that is missing.",
			"No applicable alternative named in the evidence is missing."),
		"helper_present", noul(
			"Does `evidence` identify an existing helper for the operation in `decision`?",
			"A named existing helper addresses this operation.",
			"No existing helper for this operation is identified."),
		"helper_in_options", noul(
			"Assume the evidence identifies an existing helper: do `options` include reusing that helper?",
			"An option describes using the identified helper.",
			"Every option omits use of the identified helper."),
		"argued", noul("Do `decision`, the option descriptions, or `unverified_claims` use loaded wording "+
			"to promote an option? `facts` contains verified user quotations. Treat those quotations "+
			"as requirements, not advocacy by the agent. A neutral source citation is not advocacy.",
			"They contain advocacy, loaded wording, or claims that one option is better.",
			"They are neutral statements."),
		"argued_field", json.NewObject("type", "choice", "instructions",
			"Assume the request contains loaded wording: identify the field that needs a neutral rewrite. "+
				"A source citation or authentic user quotation is not agent advocacy.",
			"criteria", argued),
		"routine_operation", noul(
			"Does `decision` describe a routine operation or a correction within an already selected approach?",
			"It runs checks, formats text, records status, or reuses an existing helper or test pattern "+
				"to correct a failure while preserving the selected behavior and interfaces.",
			"It selects a new material approach or changes product behavior or interfaces."),
		"evidence_needed", json.NewObject("type", "choice", "instructions",
			"Assume information is missing: which concrete check would most help settle this choice? "+
				"Select a check supported by the current evidence, not a speculative concern.", "criteria", json.NewObject(
				"requirements", "Check the missing required user constraint. An engineering layout preference "+
					"does not require a new user instruction.",
				"implementation", "Inspect the current implementation or existing helper relevant to this decision.",
				"comparison", "Run a targeted comparison of the options against the stated requirement.",
				"origin", "Corroborate the agent's necessary claims with independent observations.")),
		"owner", ownerQuestion("`decision`"),
	)
	if missingFacts.Len() > 0 {
		criteria := missingFacts.Copy()
		criteria.Set("not_identified", "No supplied statement identifies a missing necessary fact.")
		questions.Set("missing_fact", json.NewObject("type", "choice", "instructions",
			"Assume a necessary fact for comparing `options` is missing: which supplied request statement "+
				"needs an observation or a required constraint value from `evidence`? Select the concrete "+
				"statement whose support is missing. Request statements are agent claims, not observations. "+
				"This diagnostic does not decide permission. Select not_identified if no statement names "+
				"the missing fact, or if the evidence already supports the relevant statements.",
			"criteria", criteria))
	}
	invariants, _ := scope.Value("invariants").([]any)
	for _, id := range options.Keys() {
		description := quote(options.Value(id).(string))
		questions.Set("follows_pattern_"+id, noul(
			"Assume current source shows a pattern applicable to `decision`: does this operation follow "+
				"that observed pattern? "+description,
			"It uses the observed interface or data-flow organization.",
			"It introduces a different organization. General desirability does not establish that it follows the pattern."))
		questions.Set("specified_"+id, noul(
			"Do the captured user instructions prescribe this operation for `decision`: "+description+"?",
			"A user instruction directly prescribes this operation. Equivalent behavior counts when wording differs.",
			"No instruction prescribes this operation. Mentioning an alternative or stating a general goal does not select it."))
		for index, invariant := range invariants {
			questions.Set(fmt.Sprintf("invariant_%d_%s", index, id), noul(
				"Does this proposed operation satisfy the required invariant for the stage requested by `decision`? "+
					"Operation: "+description+". Invariant: "+quote(invariant.(string))+". "+
					"For approach selection, evaluate compatibility with observed constraints. Implementation and "+
					"execution checks can be future work. Do not count proposed checks as completed proof. "+
					"A necessary current prerequisite must still be observed.",
				"The proposed operation is compatible with this invariant at the requested stage, "+
					"supported by current observations.",
				"The proposed operation conflicts with this invariant or depends on an unobserved current prerequisite."))
		}
	}
	return questions
}

// nextStep returns _NEXT[action].format(pick=pick, reasons=reasons, n=n).
//
// py: agent_decisions/gates.py _NEXT
func nextStep(action, pick, reasons string, n int) string {
	switch action {
	case "routine":
		return "Carry out the routine correction or settled instruction. No new approach consultation is needed."
	case "binding":
		return "Follow pick `" + pick + "`. It is binding for this subagent."
	case "advisory":
		return "Take pick `" + pick + "`, or override it and state in your reply the evidence that outweighs it."
	case "parent_review":
		return "The options have no decisive preference. Ask the parent to accept a reviewed eligible option. " +
			"This result does not authorize a worker choice or write."
	case "gather_evidence":
		return "The evidence is not enough to choose. Gather it with tool calls (read the code, run the " +
			"check), then call ts-decide again."
	case "ask_user":
		return "The user owns this decision. Ask the user, with the options and pick `" + pick + "` as your " +
			"recommendation."
	case "refused":
		return "Refused (" + reasons + "), attempt " + strconv.Itoa(n) + ". Repair the options or evidence before retrying. " +
			"List every viable option and use neutral descriptions with source excerpts."
	}
	panic("gates: no next step for " + action)
}

var repairable = map[string]bool{"missing_option": true, "missing_existing_helper": true,
	"pick_conflicts_with_instruction": true, NoneFit: true}

// repair returns _REPAIRS[reason] formatted, for a repairable reason.
//
// py: agent_decisions/gates.py _REPAIRS
func repair(e *expr, reason, prescribed string, helperPresent, helperInOptions any) string {
	switch reason {
	case "missing_option":
		return "Evidence names an applicable alternative that no option lists. " +
			"Add the alternative that the user or the observed source names."
	case "missing_existing_helper":
		return "Evidence names an existing helper (helper_present " + e.fixed2(helperPresent) + ") that no " +
			"option reuses (helper_in_options " + e.fixed2(helperInOptions) + "). Add an option that " +
			"reuses the helper. If the helper does not apply, state why in that option's description."
	case "pick_conflicts_with_instruction":
		return "Captured user instructions prescribe " + prescribed + ". " +
			"Follow the instruction or ask the user to change it."
	case NoneFit:
		return "No listed option fits the evidence. Add the option that the evidence supports."
	}
	panic("gates: no repair for " + reason)
}

var evidenceChecks = map[string]string{
	"requirements": "Identify the missing required constraint in captured instructions. " +
		"An engineering preference is not a required user constraint.",
	"implementation": "Read the implementation or helper for this decision and include its path in the next request.",
	"comparison":     "Run a check comparing these options against the user's requirement and include the observed result.",
	"origin":         "Corroborate the listed unverified claims with source files or the actual check, not printed assertions.",
}

// lookup returns m[key] for a decoded key, or the KeyError or TypeError that Python raises.
func (e *expr) lookup(m map[string]string, key any) string {
	name, found := e.key(key, "dict key")
	if e.err != nil {
		return ""
	}
	value, ok := m[name]
	if !found || !ok {
		e.fail("KeyError", "%s", json.Repr(key))
	}
	return value
}

// py: agent_decisions/gates.py _verdict
func verdict(action, decision string, answers *json.Object, unverified, reasons []string, refusals int,
	overCap bool, receiptID *string, observed, missingFacts *json.Object, providerLabel string, invariantOnly bool) (string, error) {
	var e expr
	pick := e.item(e.item(answers, "pick"), "choice")
	probabilities := e.items(e.item(e.item(answers, "pick"), "probabilities"))
	type ranking struct {
		option string
		key    float64
		value  any
	}
	var ranks []ranking
	for _, option := range probabilities.Keys() {
		value := probabilities.Value(option)
		ranks = append(ranks, ranking{option, e.negated(value), value})
	}
	sort.SliceStable(ranks, func(i, j int) bool { return ranks[i].key < ranks[j].key })
	ranked := make([]string, len(ranks))
	for i, rank := range ranks {
		ranked[i] = rank.option + " " + e.fixed2(rank.value)
	}
	pUser := e.get(e.item(e.item(answers, "owner"), "probabilities"), "user", 0.0)
	head := "ts-decide verdict: " + headings[action]
	if overCap && len(reasons) > 0 {
		head += " (REQUEST STILL NEEDS REPAIR)"
	}
	lines := []string{head, "decision: " + decision, "pick: " + display(pick) + "  (" + strings.Join(ranked, ", ") + ")",
		"sufficient " + e.fixed2(e.item(e.item(answers, "sufficient"), "noul")) +
			"  missing_option " + e.fixed2(e.item(e.item(answers, "missing_option"), "noul")) +
			"  argued " + e.fixed2(e.item(e.item(answers, "argued"), "noul")) + "  owner=user " + e.fixed2(pUser)}
	if receiptID != nil {
		lines = append(lines[:1], append([]string{"receipt: " + *receiptID}, lines[1:]...)...)
	}
	if len(unverified) > 0 {
		quoted := make([]string, len(unverified))
		for i, fact := range unverified {
			quoted[i], _ = json.Dumps(fact, json.Options{AllowUnicode: true})
		}
		lines = append(lines, "unverified claims (not verified as the user's words): "+strings.Join(quoted, "; "))
	}
	if contains(reasons, "argued") {
		field := e.get(e.get(answers, "argued_field", &json.Object{}), "choice", "decision or option descriptions")
		name, ok := field.(string)
		if !ok && e.err == nil {
			e.fail("AttributeError", "'%s' object has no attribute 'startswith'", json.TypeName(field))
		}
		if strings.HasPrefix(name, "option_") {
			name = "options." + name[len("option_"):] + ".description"
		}
		lines = append(lines, "repair field: "+name+". Use neutral wording and retain the same requirements.")
	}
	var prescribed []string
	for _, key := range answers.Keys() {
		if strings.HasPrefix(key, "specified_") && e.ge(e.item(answers.Value(key), "noul"), bindAt) {
			prescribed = append(prescribed, key[len("specified_"):])
		}
	}
	for _, reason := range reasons {
		if !repairable[reason] {
			continue
		}
		helperPresent := e.item(e.item(answers, "helper_present"), "noul")
		helperInOptions := e.item(e.item(answers, "helper_in_options"), "noul")
		lines = append(lines, "repair: "+repair(&e, reason, strings.Join(prescribed, ", "), helperPresent, helperInOptions))
	}
	if _, ok := answers.Get("pattern_present"); ok {
		lines = append(lines, "observed_pattern "+e.fixed2(e.item(e.item(answers, "pattern_present"), "noul"))+
			"  pick_follows_pattern "+e.fixed2(e.get(e.get(answers, "follows_pattern_"+display(pick), &json.Object{}), "noul", 0.0)))
	}
	confidence := e.item(e.item(answers, "pick"), "confidence")
	if e.lt(confidence, bindAt) {
		lines = append(lines, "preference: uncertain (confidence "+e.fixed2(confidence)+"; binding requires 0.80)")
	}
	if action == "routine" {
		lines = append(lines, "classification: exact_operation="+e.fixed2(e.get(e.get(answers, "exact_operation", &json.Object{}), "noul", 0.0))+
			"; routine_operation="+e.fixed2(e.get(e.get(answers, "routine_operation", &json.Object{}), "noul", 0.0))+
			". PROCEED does not select a material approach.")
	}
	next := nextStep(action, display(pick), strings.Join(reasons, ", "), refusals+1)
	missingChoice := func() any {
		return e.get(e.get(answers, "missing_fact", &json.Object{}), "choice", nil)
	}
	if invariantOnly {
		next = "The selected option's required invariants remain unverified. Inspect the displayed invariant checks and current observations. " +
			"This result grants no work permission."
	} else if action == "gather_evidence" &&
		is(e.get(e.get(answers, "evidence_needed", &json.Object{}), "choice", nil), "comparison") &&
		!json.Truthy(e.get(missingFacts, missingChoice(), nil)) {
		next = "Diagnose the displayed blockers. Do not repeat a completed comparison or choose from this result."
	}
	lines = append(lines, "next: "+next)
	if action == "gather_evidence" {
		lines = append(lines, "checks: sufficient="+e.fixed2(e.item(e.item(answers, "sufficient"), "noul"))+"; "+
			"unsupported="+e.fixed2(e.item(e.item(answers, "unsupported"), "noul"))+" (blocks at 0.70); "+
			"omitted_requirement="+e.fixed2(e.item(e.item(answers, "omitted_requirement"), "noul"))+" (blocks at 0.70)")
		target := e.get(e.get(answers, "evidence_needed", &json.Object{}), "choice", "comparison")
		missing := e.get(missingFacts, missingChoice(), nil)
		if json.Truthy(missing) {
			quoted, err := json.Dumps(missing, json.Options{AllowUnicode: true})
			if err != nil && e.err == nil {
				e.err = err
			}
			if invariantOnly {
				lines = append(lines, "reviewer evidence suggestion: "+quoted,
					"check: Check whether the named statement is necessary for the blocking invariant. "+
						"Retain observed facts and required constraints. Do not retry an unchanged request.")
			} else {
				lines = append(lines, "missing evidence: "+quoted,
					"check: Corroborate the named statement with a source read or the relevant tool call. "+
						"Include its path and the observed result in the next request.")
			}
		} else {
			lines = append(lines, "missing evidence: no specific missing fact was identified by "+providerLabel+".")
			if invariantOnly || is(target, "comparison") {
				lines = append(lines, "check: Review remains unresolved. Inspect the displayed blockers and invariant text. "+
					"Do not repeat a completed comparison. This result grants no work permission.")
			} else {
				lines = append(lines, "check: "+e.lookup(evidenceChecks, target))
			}
		}
		var paths []string
		for _, source := range e.iter(e.get(observed, "source_files", []any{})) {
			if path, ok := e.get(source, "path", nil).(string); ok {
				paths = append(paths, path)
			}
		}
		if (is(target, "implementation") || is(target, "origin")) && len(paths) > 0 {
			lines = append(lines, "observed sources for this check: "+strings.Join(paths[:min(8, len(paths))], ", "))
		}
	}
	if e.err != nil {
		return "", e.err
	}
	return strings.Join(lines, "\n"), nil
}

// sha256Hex returns hashlib.sha256(text.encode()).hexdigest() for ASCII text.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// uuid4Hex returns uuid.uuid4().hex.
//
// py: Lib/uuid.py uuid4 (Python 3.15)
func uuid4Hex() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", pyerr.NewOSError(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:]), nil
}

func strings2any(items []string) []any {
	list := make([]any, len(items))
	for i, item := range items {
		list[i] = item
	}
	return list
}

func thresholds() []any {
	return []any{bindAt, stopAt, followAt, refuseAt}
}

// Decide reviews a ts-decide request.
//
// py: agent_decisions/gates.py decide
func Decide(raw string, ctx Context, ask Ask) (Outcome, error) {
	decision, options, facts, scope, contracts, err := decideRequest(raw)
	if err != nil {
		if pyerr.IsInstance(err, "RequestError") {
			return Outcome{Action: "invalid", Message: "ts-decide: invalid request: " + err.Error() + "\n" + Usage}, nil
		}
		return Outcome{}, err
	}
	var e expr
	key := provenance.Normalize(decision)
	scopePaths := scope.Value("paths")
	if json.Truthy(scopePaths) {
		ids := options.Keys()
		sort.Strings(ids) // option ids are ASCII
		identity, err := json.Dumps(json.NewObject("scope", scope, "option_ids", strings2any(ids)), json.Options{SortKeys: true})
		if err != nil {
			return Outcome{}, err
		}
		key = "scope_" + sha256Hex(identity)
	}
	refusals := 0
	for _, entry := range ctx.Entries {
		if is(entry.Value("gate"), "decide") && is(entry.Value("action"), "refused") &&
			is(entry.Value("decision_key"), key) && e.equal(entry.Value("agent_id"), ctx.AgentID) {
			refusals++
		}
	}
	if e.err != nil {
		return Outcome{}, e.err
	}
	verified, unverified, err := provenance.Verify(facts, ctx.Transcript)
	if err != nil {
		return Outcome{}, err
	}
	state := json.NewObject("policy", reviewPolicy(ctx), "decision", decision, "options", options,
		"facts", strings2any(verified), "unverified_claims", strings2any(unverified), "evidence", ctx.Evidence)
	missingFacts := missingFactCandidates(decision, options, scope)
	if scope.Len() > 0 {
		state.Set("scope", scope)
	}
	record := json.NewObject("gate", "decide", "decision", decision, "decision_key", key, "state", state)
	markReviewIdentity(record, ctx)
	if len(unverified) > 0 {
		lines := []string{"ts-decide: facts must quote captured user messages. Remove the unverified " +
			"facts below. The hook supplies tool observations and source files automatically."}
		for _, fact := range unverified {
			quoted, _ := json.Dumps(fact, json.Options{AllowUnicode: true})
			lines = append(lines, "- "+quoted)
		}
		return Outcome{Action: "invalid", Message: strings.Join(lines, "\n"), Record: record}, nil
	}
	if len(contracts) > 0 {
		observed := &json.Object{}
		resolved := map[string]string{}
		for _, item := range e.iter(e.get(ctx.Evidence, "source_files", []any{})) {
			if is(e.get(item, "trust", nil), "verified_file_observation") {
				path, found := e.key(e.item(item, "path"), "dict key") // another hashable key matches no contract
				digest := e.item(item, "sha256")
				if found {
					observed.Set(path, digest)
					for _, name := range evidence.SourcePaths(item.(*json.Object)) {
						observed.Set(name, digest)
						resolved[name] = path
					}
				}
			}
		}
		if e.err != nil {
			return Outcome{}, e.err
		}
		var missing []string
		for _, path := range contracts {
			if _, ok := observed.Get(path); !ok {
				missing = append(missing, path)
			}
		}
		if len(missing) > 0 {
			message := "ts-decide verdict: GATHER EVIDENCE\ncheck: Read the contract files " +
				"and include their paths in the next request: " + strings.Join(missing, ", ")
			if metadata, ok := ctx.Evidence.Value("metadata").(*json.Object); ok {
				if root, ok := metadata.Value("source_root").(string); ok {
					message += "\nsource root: " + quote(root)
				}
			}
			return Outcome{Action: "gather_evidence", Message: message, Record: record}, nil
		}
		observedContracts := &json.Object{}
		for _, path := range contracts {
			observedContracts.Set(path, observed.Value(path))
			if target, ok := resolved[path]; ok {
				observedContracts.Set(target, observed.Value(path))
			}
		}
		record.Set("contracts", observedContracts)
		state.Set("contracts", observedContracts)
	}
	if json.Truthy(scopePaths) {
		outcome, reused, err := reuseReview(ctx, record, decision, key, options, scope, verified)
		if err != nil || reused {
			return outcome, err
		}
	}
	answers, err := ask(state, decideQuestions(options, scope, missingFacts))
	if err != nil {
		var failure *jev.JevError
		if !errors.As(err, &failure) {
			return Outcome{}, err
		}
		open, err := ledger.FailOpen(ctx.Host)
		if err != nil {
			return Outcome{}, err
		}
		next := "Required review is unavailable. Restore the service and consult again before choosing or writing."
		if open {
			next = "Decide yourself and state the evidence for your choice."
		}
		return Outcome{Action: "unverified", Message: "ts-decide: decision unverified (" + reviewer(ctx).Name + " " + failure.Error() + "). " + next,
			Record: record}, nil
	}
	return judge(ctx, answers, record, decision, options, scope, refusals, missingFacts)
}

// decideRequest is the validation in decide's try block.
//
// py: agent_decisions/gates.py decide
func decideRequest(raw string) (decision string, options *json.Object, facts []string, scope *json.Object,
	contracts []string, err error) {
	decision, options, facts, err = ParseRequest(raw)
	if err != nil {
		return
	}
	if scope, err = RequestScope(raw); err != nil {
		return
	}
	request, _ := json.Loads(raw) // ParseRequest has decoded raw
	value, ok := request.(*json.Object).Get("contracts")
	if !ok {
		value = []any{}
	}
	text, err := json.Dumps(json.NewObject("scope", json.NewObject("paths", value)), json.Options{})
	if err != nil {
		return
	}
	checked, err := RequestScope(text)
	if err != nil {
		return
	}
	for _, path := range checked.Value("paths").([]any) {
		contracts = append(contracts, path.(string))
	}
	invariants, _ := scope.Value("invariants").([]any)
	if len(contracts) > 8 || hasSuffix(contracts, "/") {
		err = requestError("contracts must contain at most eight repository-relative files")
	} else if options.Len()*len(invariants) > 256 {
		err = requestError("a request can contain at most 256 invariant checks; split the review")
	}
	for i, path := range contracts {
		contracts[i] = pathlib.Str(path)
	}
	return
}

func hasSuffix(items []string, suffix string) bool {
	for _, item := range items {
		if strings.HasSuffix(item, suffix) {
			return true
		}
	}
	return false
}

// reuseReview fingerprints a scoped review and returns the prior verdict when nothing it compared changed.
//
// py: agent_decisions/gates.py decide
func reuseReview(ctx Context, record *json.Object, decision, key string, options, scope *json.Object,
	verified []string) (Outcome, bool, error) {
	var e expr
	unique := map[string]bool{}
	var observations []string
	for _, call := range e.iter(e.get(ctx.Evidence, "tool_calls_newest_first", []any{})) {
		fields := &json.Object{}
		for _, name := range []string{"tool", "input", "output", "trust", "execution"} {
			fields.Set(name, e.get(call, name, nil))
		}
		if e.err != nil {
			return Outcome{}, false, e.err
		}
		text, err := json.Dumps(fields, json.Options{SortKeys: true})
		if err != nil {
			return Outcome{}, false, err
		}
		if !unique[text] {
			unique[text] = true
			observations = append(observations, text)
		}
	}
	var sources []string
	for _, source := range e.iter(e.get(ctx.Evidence, "source_files", []any{})) {
		text, err := json.Dumps(source, json.Options{SortKeys: true})
		if err != nil {
			return Outcome{}, false, err
		}
		sources = append(sources, text)
	}
	if e.err != nil {
		return Outcome{}, false, e.err
	}
	sort.Strings(observations) // json.dumps output is ASCII
	sort.Strings(sources)
	mode, err := ledger.ReviewMode(ctx.Host)
	if err != nil {
		return Outcome{}, false, err
	}
	contracts, ok := record.Get("contracts")
	if !ok {
		contracts = &json.Object{}
	}
	users, ok := ctx.Evidence.Get("user_messages")
	if !ok {
		users = []any{}
	}
	comparison := json.NewObject("policy", reviewPolicy(ctx), "gate_contract", reviewContract,
		"diagnostic_contract", "missing-fact-1", "repo_id", ctx.RepoID, "thresholds", thresholds(),
		"options", options, "scope", scope, "facts", strings2any(verified),
		"model", reviewer(ctx).Model, "contracts", contracts, "review_mode", mode,
		"users", users, "brief", ctx.Evidence.Value("brief"),
		"observations", strings2any(observations), "sources", strings2any(sources))
	markReviewIdentity(comparison, ctx)
	text, err := json.Dumps(comparison, json.Options{SortKeys: true})
	if err != nil {
		return Outcome{}, false, err
	}
	fingerprint := sha256Hex(text)
	record.Set("evidence_fingerprint", fingerprint)
	var prior *json.Object
	for i := len(ctx.Entries) - 1; i >= 0 && prior == nil; i-- {
		entry := ctx.Entries[i]
		_, isVerdict := entry.Value("verdict").(string)
		if is(entry.Value("gate"), "decide") && is(entry.Value("host"), ctx.Host) &&
			e.equal(entry.Value("agent_id"), ctx.AgentID) && is(entry.Value("decision_key"), key) &&
			is(entry.Value("evidence_fingerprint"), fingerprint) && e.in(entry.Value("action"), headingActions) && isVerdict {
			prior = entry
		}
	}
	if e.err != nil || prior == nil {
		return Outcome{}, false, e.err
	}
	if is(prior.Value("action"), "routine") && !is(prior.Value("decision"), decision) {
		return Outcome{}, false, nil
	}
	answers := e.get(prior, "answers", &json.Object{})
	repairsQuestion := is(prior.Value("action"), "refused") && !is(prior.Value("decision"), decision) &&
		e.ge(e.get(e.get(answers, "argued", &json.Object{}), "noul", 0.0), refuseAt) &&
		is(e.get(e.get(e.get(prior, "answers", &json.Object{}), "argued_field", &json.Object{}), "choice", "decision"), "decision")
	if e.err != nil || repairsQuestion {
		return Outcome{}, false, e.err
	}
	return Outcome{Action: prior.Value("action").(string),
		Message: prior.Value("verdict").(string) + "\nreused: no changed options, constraints, or observations."}, true, nil
}

// judge turns Jev's answers to a decide request into a verdict.
//
// py: agent_decisions/gates.py decide
func judge(ctx Context, answers, record *json.Object, decision string, options, scope *json.Object, refusals int,
	missingFacts *json.Object) (Outcome, error) {
	var e expr
	noulOf := func(name string) any { return e.item(e.item(answers, name), "noul") }
	pick := e.item(e.item(answers, "pick"), "choice")
	prescribed := map[string]bool{}
	for _, id := range options.Keys() {
		if e.ge(noulOf("specified_"+id), bindAt) {
			prescribed[id] = true
		}
	}
	exact := e.get(e.get(answers, "specified_"+display(pick), &json.Object{}), "noul", 0.0)
	if e.err != nil {
		return Outcome{}, e.err
	}
	answers.Set("exact_operation", json.NewObject("type", "noul", "noul", exact))
	basisAt := bindAt
	if ctx.AgentID == nil && e.ge(e.get(e.item(e.item(answers, "owner"), "probabilities"), "agent", 0.0), stopAt) {
		basisAt = followAt // advisory engineering judgment remains with the main agent
	}
	patternBasis := e.ge(noulOf("pattern_present"), basisAt) &&
		e.ge(e.get(e.get(answers, "follows_pattern_"+display(pick), &json.Object{}), "noul", 0.0), basisAt)
	hasBasis := e.ge(noulOf("sufficient"), basisAt) || patternBasis
	invariants, _ := scope.Value("invariants").([]any)
	checksOf := func(id string) []string {
		checks := make([]string, len(invariants))
		for index := range invariants {
			checks[index] = fmt.Sprintf("invariant_%d_%s", index, id)
		}
		return checks
	}
	eligible := []string{}
	for _, id := range options.Keys() {
		all := true
		for _, check := range checksOf(id) {
			if !e.ge(noulOf(check), bindAt) {
				all = false
				break
			}
		}
		if all {
			eligible = append(eligible, id)
		}
	}
	// invariant_checks.get(pick, ()) hashes pick.
	pickID, found := e.key(pick, "dict key")
	var pickChecks []string
	if _, ok := options.Get(pickID); found && ok {
		pickChecks = checksOf(pickID)
	}
	var violated []string
	for _, check := range pickChecks {
		if e.le(noulOf(check), unbound) {
			violated = append(violated, check)
		}
	}
	hits := []struct {
		name string
		hit  bool
	}{
		{"missing_option", e.ge(noulOf("missing_option"), refuseAt)},
		{"missing_existing_helper", len(prescribed) == 0 && e.ge(noulOf("helper_present"), bindAt) &&
			e.le(noulOf("helper_in_options"), unbound)},
		{"argued", e.ge(noulOf("argued"), refuseAt)},
		{"pick_conflicts_with_instruction", len(prescribed) > 0 && !e.in(pick, prescribed)},
		{NoneFit, is(pick, NoneFit)},
	}
	var reasons []string
	for _, item := range hits {
		if item.hit {
			reasons = append(reasons, item.name)
		}
	}
	reasons = append(reasons, violated...)
	overCap := refusals >= policy.RefusalCap
	userOwned := e.ge(e.get(e.item(e.item(answers, "owner"), "probabilities"), "user", 0.0), bindAt)
	unready := func() any { return e.max(noulOf("unsupported"), noulOf("omitted_requirement")) }
	blocked := func() bool { return e.ge(unready(), stopAt) }
	onlyNoneFit := len(reasons) == 1 && reasons[0] == NoneFit
	if userOwned && onlyNoneFit {
		reasons = nil // the user chooses; an option the agent left out is theirs to name
	} else if onlyNoneFit && (!hasBasis || blocked()) {
		reasons = nil // missing context needs an observation, not another list of options
	}
	var action string
	switch {
	case len(reasons) > 0:
		action = "refused"
	case e.ge(e.max(noulOf("exact_operation"), noulOf("routine_operation")), bindAt) && e.lt(unready(), stopAt):
		action = "routine"
	case userOwned:
		action = "ask_user"
	case !hasBasis || blocked() || is(pick, NoneFit) || !(found && contains(eligible, pickID)):
		action = "gather_evidence"
	case json.Truthy(ctx.AgentID) && e.lt(e.item(e.item(answers, "pick"), "confidence"), bindAt):
		action = "parent_review"
	case json.Truthy(ctx.AgentID):
		action = "binding"
	default:
		action = "advisory"
	}
	if e.err != nil {
		return Outcome{}, e.err
	}
	var receiptID *string
	if action == "advisory" || action == "binding" {
		id, err := uuid4Hex()
		if err != nil {
			return Outcome{}, err
		}
		id = "review_" + id
		receiptID = &id
	}
	invariantOnly := action == "gather_evidence" && hasBasis && !blocked() &&
		found && !is(pick, NoneFit) && !contains(eligible, pickID)
	message, err := verdict(action, decision, answers, nil, reasons, refusals, overCap, receiptID, ctx.Evidence, missingFacts, reviewerName(ctx), invariantOnly)
	if err != nil {
		return Outcome{}, err
	}
	if action == "gather_evidence" || action == "refused" {
		var pending []string
		for index, check := range pickChecks {
			if e.lt(noulOf(check), bindAt) {
				quoted, _ := json.Dumps(invariants[index], json.Options{AllowUnicode: true})
				pending = append(pending, check+"="+e.fixed2(noulOf(check))+" (requires 0.80): "+quoted)
			}
		}
		if e.err != nil {
			return Outcome{}, e.err
		}
		if len(pending) > 0 {
			message += "\ncheck invariants: " + strings.Join(pending, "; ")
		}
	}
	fields := record.Copy()
	fields.Set("answers", answers)
	fields.Set("pick", pick)
	fields.Set("eligible_options", strings2any(eligible))
	fields.Set("eligibility_checked", len(invariants) > 0)
	if receiptID != nil {
		fields.Set("receipt_id", *receiptID)
		fields.Set("repo_id", ctx.RepoID)
	}
	return Outcome{Action: action, Message: message, Record: fields}, nil
}

// Misuse explains that ts-decide must be the whole command.
//
// py: agent_decisions/gates.py MISUSE
const Misuse = "ts-decide must be the whole command, not part of a pipeline, a compound command, or a redirect.\n" +
	Usage

const punctuation = "();<>|&\n"

var stdinShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true, "ksh": true,
	"csh": true, "tcsh": true, "fish": true, "pwsh": true, "powershell": true}

// envAssignment is _ENV_ASSIGNMENT.fullmatch with re.S: the rest after = is anything. The classes are ASCII.
var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var executionWrappers = map[string]map[string]bool{
	"env":     {"-u": true, "--unset": true, "-C": true, "--chdir": true, "-P": true},
	"command": {}, "exec": {"-a": true}, "time": {"-f": true, "--format": true, "-o": true, "--output": true},
	"xargs": {"-a": true, "--arg-file": true, "-E": true, "-I": true, "-J": true, "-L": true, "-n": true, "-P": true,
		"-R": true, "-s": true, "-S": true, "--max-args": true, "--max-procs": true, "--max-chars": true},
}

// isWordChar reports whether the character at the start of s matches \w in a Python str pattern.
func isWordChar(s string) bool {
	_, size := pystr.DecodeRune(s)
	return s[0] == '_' || pystr.IsAlnum(s[:size])
}

// wordEnd returns the end of the run of \w characters at s[i:].
func wordEnd(s string, i int) int {
	for i < len(s) && isWordChar(s[i:]) {
		_, size := pystr.DecodeRune(s[i:])
		i += size
	}
	return i
}

// spaceEnd returns the end of the run of \s characters at s[i:].
func spaceEnd(s string, i int) int {
	for i < len(s) {
		r, size := pystr.DecodeRune(s[i:])
		if !pystr.IsSpace(r) {
			break
		}
		i += size
	}
	return i
}

// blankEnd returns the end of the run of [ \t] at s[i:].
func blankEnd(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// heredocRequest returns group 3 of _HEREDOC.fullmatch(s), the request in a ts-decide heredoc:
// \s*(?:\S*/)?ts-decide\s*<<-?\s*(['"]?)(\w+)\1[ \t]*\n(.*?)\n[ \t]*\2\s* with re.S.
//
// Only one placement of ts-decide in the first word can be followed by <<, so the greedy prefix is tried
// from the last / back to none. Every later quantifier but the body is forced; the body is the shortest
// that leaves a closing line.
//
// py: agent_decisions/gates.py _HEREDOC (hand-port: backreferences)
func heredocRequest(s string) (string, bool) {
	start := spaceEnd(s, 0)
	end := start
	for end < len(s) {
		r, size := pystr.DecodeRune(s[end:])
		if pystr.IsSpace(r) {
			break
		}
		end += size
	}
	for slash := strings.LastIndexByte(s[start:end], '/'); ; slash = strings.LastIndexByte(s[start:start+slash], '/') {
		at := start
		if slash >= 0 {
			at = start + slash + 1
		}
		if request, ok := heredocAt(s, at); ok {
			return request, true
		}
		if slash < 0 {
			return "", false
		}
	}
}

// heredocAt matches _HEREDOC from ts-decide at s[i:].
func heredocAt(s string, i int) (string, bool) {
	if !strings.HasPrefix(s[i:], "ts-decide") {
		return "", false
	}
	i = spaceEnd(s, i+len("ts-decide"))
	if !strings.HasPrefix(s[i:], "<<") {
		return "", false
	}
	i += 2
	if strings.HasPrefix(s[i:], "-") {
		i++
	}
	i = spaceEnd(s, i)
	quote := ""
	if i < len(s) && (s[i] == '\'' || s[i] == '"') {
		quote = s[i : i+1]
		i++
	}
	wordStart := i
	i = wordEnd(s, i)
	if i == wordStart || !strings.HasPrefix(s[i:], quote) {
		return "", false
	}
	word := s[wordStart:i]
	i = blankEnd(s, i+len(quote))
	if !strings.HasPrefix(s[i:], "\n") {
		return "", false
	}
	body := i + 1
	for at := body; at < len(s); at++ {
		if s[at] != '\n' {
			continue
		}
		closing := blankEnd(s, at+1)
		if strings.HasPrefix(s[closing:], word) && spaceEnd(s, closing+len(word)) == len(s) {
			return s[body:at], true
		}
	}
	return "", false
}

// dataHeredoc is a _DATA_HEREDOC.fullmatch: ([^\n]*?)<<(-?)[ \t]*(['"])(\w+)\3[ \t]*.
type dataHeredoc struct {
	command   string // group 1
	stripTabs bool   // group 2
	delimiter string // group 4
}

// matchDataHeredoc matches _DATA_HEREDOC against a line without \n. The lazy prefix ends at the first << that
// the forced rest matches.
//
// py: agent_decisions/gates.py _DATA_HEREDOC (hand-port: backreference)
func matchDataHeredoc(line string) (dataHeredoc, bool) {
	for k := strings.Index(line, "<<"); k >= 0; {
		i := k + 2
		stripTabs := strings.HasPrefix(line[i:], "-")
		if stripTabs {
			i++
		}
		i = blankEnd(line, i)
		if i < len(line) && (line[i] == '\'' || line[i] == '"') {
			quote := line[i]
			wordStart := i + 1
			end := wordEnd(line, wordStart)
			if end > wordStart && end < len(line) && line[end] == quote && blankEnd(line, end+1) == len(line) {
				return dataHeredoc{line[:k], stripTabs, line[wordStart:end]}, true
			}
		}
		next := strings.Index(line[k+1:], "<<")
		if next < 0 {
			break
		}
		k += 1 + next
	}
	return dataHeredoc{}, false
}

// commandInput is _CommandInput: after a comment, the newline stays in the stream as a separator.
//
// py: agent_decisions/gates.py _CommandInput
type commandInput struct{ *shlex.StringStream }

// py: agent_decisions/gates.py _CommandInput.readline
func (c commandInput) ReadLine() string {
	line := c.StringStream.ReadLine()
	if strings.HasSuffix(line, "\n") {
		if _, err := c.Seek(c.Tell()-1, io.SeekStart); err != nil {
			panic(err) // a StringIO seek to a known position cannot fail
		}
	}
	return line
}

// py: agent_decisions/gates.py _execution_head
func executionHead(argv []string, start int) string {
	// shortcut: Recognize literal wrappers; extend when a native trace needs more shell syntax.
	index := start
	for index < len(argv) {
		arg := argv[index]
		if envAssignment.MatchString(arg) {
			index++
			continue
		}
		wrapper := basename(arg)
		flags, ok := executionWrappers[wrapper]
		if !ok {
			return wrapper
		}
		index++
		options := true
	wrapped:
		for index < len(argv) {
			arg := argv[index]
			switch {
			case options && arg == "--":
				options = false
				index++
			case wrapper == "env" && envAssignment.MatchString(arg):
				index++
			case options && wrapper == "command" && strings.HasPrefix(arg, "-") && strings.ContainsAny(arg[1:], "vV"):
				return "" // These modes locate the command without executing it.
			case options && wrapper == "env" && (strings.HasPrefix(arg, "-S") || arg == "--split-string" ||
				strings.HasPrefix(arg, "--split-string=")):
				separate := arg == "-S" || arg == "--split-string"
				if separate && index+1 >= len(argv) {
					return ""
				}
				var text string
				switch {
				case separate:
					text = argv[index+1]
				case strings.HasPrefix(arg, "-S"):
					text = arg[2:]
				default:
					text = arg[strings.IndexByte(arg, '=')+1:]
				}
				split, err := shlex.Split(text)
				if err != nil {
					return ""
				}
				consumed := 1
				if separate {
					consumed = 2
				}
				argv = append(append(append([]string(nil), argv[:index]...), split...), argv[index+consumed:]...)
			case options && strings.HasPrefix(arg, "-"):
				if flags[arg] {
					index += 2
				} else {
					index++
				}
			default:
				break wrapped
			}
		}
	}
	return ""
}

// CommandOf returns the shell command of a tool input.
//
// py: agent_decisions/gates.py command_of
func CommandOf(toolInput *json.Object) string {
	command, ok := toolInput.Get("command")
	if !ok {
		command, ok = toolInput.Get("cmd")
		if !ok {
			command = ""
		}
	}
	if list, ok := command.([]any); ok {
		argv := make([]string, len(list))
		for i, arg := range list {
			argv[i] = display(arg)
		}
		if len(argv) == 3 && (argv[1] == "-c" || argv[1] == "-lc") {
			return argv[2]
		}
		return shlex.Join(argv)
	}
	text, _ := command.(string)
	return text
}

// isPunctuation reports set(token) <= _PUNCTUATION; it holds for "".
func isPunctuation(token string) bool {
	return strings.Trim(token, punctuation) == ""
}

// Classify returns ("decide", request) for a ts-decide call, ("misuse", "") for ts-decide inside a larger
// command, and ("other", "") otherwise.
//
// py: agent_decisions/gates.py classify
func Classify(command string) (kind, request string) {
	if request, ok := heredocRequest(strings.ReplaceAll(command, "\r\n", "\n")); ok {
		return "decide", request
	}
	lines := pystr.SplitLines(pystr.Strip(command))
	for len(lines) > 0 && (pystr.Strip(lines[0]) == "" || strings.HasPrefix(pystr.LStrip(lines[0]), "#")) {
		lines = lines[1:]
	}
	if len(lines) > 0 {
		if header, ok := matchDataHeredoc(lines[0]); ok {
			lexer := shlex.New(header.command, true, shlex.DefaultPunctuation)
			lexer.WhitespaceSplit = true
			argv, err := lexer.Tokens()
			if err != nil {
				argv = nil
			}
			head := ""
			if len(argv) > 0 {
				head = executionHead(argv, 0)
			}
			if len(argv) > 0 && !stdinShells[head] && head != "ts-decide" && !anyPunctuation(argv) {
				// A complete quoted heredoc supplies data; shells still execute their stdin.
				end := -1
				for index, line := range lines[1:] {
					if header.stripTabs {
						line = strings.TrimLeft(line, "\t")
					}
					if line == header.delimiter {
						end = index + 1
						break
					}
				}
				if end >= 0 && !anyCode(lines[end+1:]) {
					command = header.command
				}
			}
		}
	}
	var tokens []string
	quoteError := false
	lexer := shlex.NewStream(commandInput{shlex.NewStringStream(pystr.Strip(command))}, true, punctuation)
	lexer.Whitespace = " \t\r"
	lexer.WhitespaceSplit = true
	for {
		token, ok, err := lexer.GetToken()
		if err != nil {
			quoteError = true // interpreter data can use quoting that is not shell syntax
			break
		}
		if !ok {
			break
		}
		tokens = append(tokens, token)
	}
	called := false
	for i := range tokens {
		if (i == 0 || isPunctuation(tokens[i-1])) && executionHead(tokens, i) == "ts-decide" {
			called = true
			break
		}
	}
	if !called {
		return "other", ""
	}
	if !quoteError && len(tokens) == 2 && basename(tokens[0]) == "ts-decide" {
		return "decide", tokens[1]
	}
	return "misuse", ""
}

// anyPunctuation reports any(arg and set(arg) <= _PUNCTUATION for arg in argv).
func anyPunctuation(argv []string) bool {
	for _, arg := range argv {
		if arg != "" && isPunctuation(arg) {
			return true
		}
	}
	return false
}

// anyCode reports whether a line is neither blank nor a comment.
func anyCode(lines []string) bool {
	for _, line := range lines {
		if pystr.Strip(line) != "" && !strings.HasPrefix(pystr.LStrip(line), "#") {
			return true
		}
	}
	return false
}

// brace is _BRACE; it has no lookaround or backreference, so RE2's leftmost-first match is Python's.
var brace = regexp.MustCompile(`\{([^{},]*(?:,[^{},]*)+)\}`) // commas split the parts, so a failed match backtracks linearly

// expandHome returns _HOME_VARIABLE.sub(home, token) for \$(?:\{HOME\}|HOME\b), where \b is Unicode.
//
// py: agent_decisions/gates.py _HOME_VARIABLE (hand-port: Unicode \b)
func expandHome(token, home string) string {
	var out strings.Builder
	last := 0
	for i := 0; i < len(token); i++ {
		if token[i] != '$' {
			continue
		}
		end := 0
		switch rest := token[i+1:]; {
		case strings.HasPrefix(rest, "{HOME}"):
			end = i + 1 + len("{HOME}")
		case strings.HasPrefix(rest, "HOME") && (len(rest) == 4 || !isWordChar(rest[4:])):
			end = i + 1 + len("HOME")
		default:
			continue
		}
		out.WriteString(token[last:i])
		out.WriteString(home)
		last = end
		i = end - 1
	}
	if last == 0 {
		return token
	}
	out.WriteString(token[last:])
	return out.String()
}

// braces appends the shell comma-brace expansions of word to words, innermost first, until words holds
// limit words, so a {a,b} spelling of a path still names it. frame is the generator's Python frame.
//
// py: agent_decisions/gates.py _braces
func braces(word string, words []string, limit, frame int) ([]string, error) {
	if frame > recursionLimit {
		return nil, recursionError()
	}
	match := brace.FindStringSubmatchIndex(word)
	if match == nil {
		return append(words, word), nil
	}
	for _, part := range strings.Split(word[match[2]:match[3]], ",") {
		if len(words) >= limit {
			break
		}
		var err error
		if words, err = braces(word[:match[0]]+part+word[match[1]:], words, limit, frame+1); err != nil {
			return nil, err
		}
	}
	return words, nil
}

// globReaches reports whether glob path components can name root or a path below it. ** spans directories.
// frame is the call's Python frame; fnmatchcase adds one. Its pattern-cache misses run deeper Python
// frames, which this does not count.
//
// py: agent_decisions/gates.py _glob_reaches
func globReaches(pattern, root []string, frame int) (bool, error) {
	if frame > recursionLimit {
		return false, recursionError()
	}
	if len(root) == 0 {
		return true, nil
	}
	if len(pattern) == 0 {
		return false, nil // an ancestor of root, allowed as a literal ancestor is
	}
	if pattern[0] == "**" {
		if reached, err := globReaches(pattern[1:], root, frame+1); reached || err != nil {
			return reached, err
		}
		return globReaches(pattern, root[1:], frame+1)
	}
	if frame+1 > recursionLimit {
		return false, recursionError()
	}
	if !fnmatch.FnmatchCase(root[0], pattern[0]) {
		return false, nil
	}
	return globReaches(pattern[1:], root[1:], frame+1)
}

var pathKeys = map[string]bool{"path": true, "paths": true, "file": true, "files": true, "directory": true,
	"cwd": true, "workdir": true}

// patchFile is the patch file-header findall pattern; RE2 (?m) ^, $ and . match Python's.
var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)

func patchPaths(command string) []string {
	var paths []string
	for _, match := range patchFile.FindAllStringSubmatch(command, -1) {
		paths = append(paths, match[1])
	}
	return paths
}

// joinPath returns str(Path(base) / Path(p)) for normalized paths.
func joinPath(base, p string) string {
	switch {
	case isAbs(p):
		return p
	case p == ".":
		return base
	}
	return pathlib.Join(base, p)
}

// Guard gives a clear error for direct private-path access; OS isolation handles computed paths. cwd is the
// payload's cwd value; frame is guard's Python frame (5 under hook.py).
//
// py: agent_decisions/gates.py guard
func Guard(toolInput *json.Object, cwd any, frame int) (*Outcome, error) {
	stateDir, err := policy.StateDir()
	if err != nil {
		return nil, err
	}
	stateRoot, err := resolve(stateDir)
	if err != nil {
		return nil, err
	}
	homeDir, err := home()
	if err != nil {
		return nil, err
	}
	defaultRoot, err := resolve(pathlib.Join(homeDir, ".local/state/agent-decisions"))
	if err != nil {
		return nil, err
	}
	var paths []string
	var collect func(value any, key string, frame int) error
	collect = func(value any, key string, frame int) error {
		if frame > recursionLimit {
			return recursionError()
		}
		switch value := value.(type) {
		case *json.Object:
			for _, name := range value.Keys() {
				if err := collect(value.Value(name), pystr.Lower(name), frame+1); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range value {
				if err := collect(item, key, frame+1); err != nil {
					return err
				}
			}
		case string:
			if pathKeys[key] || strings.HasSuffix(key, "_path") {
				paths = append(paths, value)
			}
		}
		return nil
	}
	if err := collect(toolInput, "", frame+1); err != nil {
		return nil, err
	}
	base, err := pathOf(cwd)
	if err != nil {
		return nil, err
	}
	command := CommandOf(toolInput)
	if strings.HasPrefix(pystr.LStrip(command), "*** Begin Patch") {
		paths = append(paths, patchPaths(command)...)
	} else if command != "" {
		lexer := shlex.New(command, true, shlex.DefaultPunctuation)
		lexer.WhitespaceSplit = true
		tokens, err := lexer.Tokens()
		if err == nil { // the host diagnoses invalid shell syntax
			home, err := home()
			if err != nil {
				return nil, err
			}
			for i, token := range tokens {
				// ponytail: 64 brace expansions per word; OS isolation covers longer spellings
				words, err := braces(expandHome(token, home), nil, 64, frame+1)
				if err != nil {
					return nil, err
				}
				for _, word := range words {
					if strings.Contains(word, "\n") || !(strings.Contains(word, "/") ||
						hasAnyPrefix(word, "./", "../", "~")) {
						continue
					}
					expanded, err := ospath.ExpandUser(word)
					if err != nil {
						return nil, err
					}
					candidate := joinPath(base, pathlib.Str(expanded))
					paths = append(paths, candidate)
					if i > 0 && tokens[i-1] == "cd" {
						base = candidate
					}
				}
			}
		}
	}
	roots := []string{stateRoot, defaultRoot}
	if darwin {
		for i, root := range roots {
			roots[i] = pystr.Casefold(root)
		}
	}
	for _, value := range paths {
		expanded, err := ospath.ExpandUser(value)
		if err != nil {
			return nil, err
		}
		path := pathlib.Str(expanded)
		if !isAbs(path) {
			dir, err := pathOf(cwd)
			if err != nil {
				return nil, err
			}
			path = joinPath(dir, path)
		}
		text, err := resolve(path)
		if err != nil {
			return nil, err
		}
		if darwin {
			text = pystr.Casefold(text)
		}
		denied := false
		for _, root := range roots {
			if text == root || strings.HasPrefix(text, root+"/") {
				denied = true
			}
		}
		if !denied && strings.ContainsAny(text, "*?[") {
			for _, root := range roots { // the any() generator runs at frame+1
				reached, err := globReaches(parts(text), parts(root), frame+2)
				if err != nil {
					return nil, err
				}
				if reached {
					denied = true
					break
				}
			}
		}
		if denied {
			return &Outcome{Action: "deny", Message: "agent-decisions: tool calls may not read or write the decision ledger or verdict " +
				"files. ts-decide returns its verdict directly.", Record: json.NewObject("gate", "guard")}, nil
		}
	}
	return nil, nil
}

// cachedAnswers reuses a signed result only when all evidence and decisions are unchanged. The result is
// any decoded value, or nil for None.
//
// py: agent_decisions/gates.py _cached_answers
func cachedAnswers(ctx Context, gate string, state *json.Object) (any, error) {
	contract, err := cacheContract(ctx)
	if err != nil {
		return nil, err
	}
	var e expr
	for i := len(ctx.Entries) - 1; i >= 0; i-- {
		entry := ctx.Entries[i]
		if !is(entry.Value("host"), ctx.Host) || !e.equal(entry.Value("cache_contract"), contract) {
			if e.err != nil {
				return nil, e.err
			}
			continue
		}
		own := is(entry.Value("gate"), gate) && e.equal(entry.Value("agent_id"), ctx.AgentID)
		if _, isDict := entry.Value("answers").(*json.Object); own && e.equal(entry.Value("state"), state) && isDict {
			return entry.Value("answers"), nil
		}
		if own {
			chunks, ok := entry.Get("chunks")
			if !ok {
				chunks = []any{}
			}
			for _, chunk := range e.iter(chunks) {
				if e.equal(e.get(chunk, "state", nil), state) {
					return e.get(chunk, "answers", nil), e.err
				}
			}
		}
		if e.err != nil {
			return nil, e.err
		}
	}
	return nil, nil
}

// py: agent_decisions/gates.py _cache_contract
func cacheContract(ctx Context) (*json.Object, error) {
	mode, err := ledger.ReviewMode(ctx.Host)
	if err != nil {
		return nil, err
	}
	contract := json.NewObject("version", reviewContract, "model", reviewer(ctx).Model, "repo_id", ctx.RepoID,
		"review_mode", mode, "policy", reviewPolicy(ctx), "thresholds", thresholds())
	markReviewIdentity(contract, ctx)
	return contract, nil
}

// dumpsLen returns len(json.dumps(v)); the output is ASCII.
func dumpsLen(v any) (int, error) {
	text, err := json.Dumps(v, json.Options{})
	return len(text), err
}

// fragments splits a long string into consecutive pieces whose JSON encoding fits half a clip, ending
// each piece but the last after a newline where one falls inside it.
//
// py: agent_decisions/gates.py _pending_parts
func fragments(value string) []string {
	const half = policy.ClipChars / 2
	offsets := []int{} // offsets[i] is the byte offset of character i; the last entry is len(value)
	for i := 0; i < len(value); {
		offsets = append(offsets, i)
		_, size := pystr.DecodeRune(value[i:])
		i += size
	}
	n := len(offsets)
	offsets = append(offsets, len(value))
	slice := func(start, end int) string { return value[offsets[start]:offsets[end]] }
	var pieces []string
	for start := 0; start < n; {
		end := min(start+half, n)
		for {
			size, _ := dumpsLen(slice(start, end))
			if size <= half {
				break
			}
			end = start + max(1, (end-start)/2)
		}
		if newline := strings.LastIndexByte(slice(start, end), '\n'); newline >= 0 {
			index := sort.SearchInts(offsets, offsets[start]+newline)
			if index > start && end < n {
				end = index + 1
			}
		}
		pieces = append(pieces, slice(start, end))
		start = end
	}
	return pieces
}

// pendingParts splits a tool input into parts whose state fits a review request. frame is _pending_parts's
// Python frame.
//
// py: agent_decisions/gates.py _pending_parts
func pendingParts(toolInput *json.Object, frame int) ([]*json.Object, error) {
	serialized, err := json.Dumps(toolInput, json.Options{})
	if err != nil {
		return nil, err
	}
	if len(serialized) <= policy.ClipChars {
		return []*json.Object{toolInput}, nil
	}
	if len(serialized) > 256_000 {
		return nil, requestError("tool input exceeds 256,000 characters; submit smaller edits")
	}
	// Keep field identities through chunking. An old_string is existing code;
	// cutting raw JSON strings made it indistinguishable from a proposed change.
	var fields []any
	var visit func(value any, path []any, frame int) error
	visit = func(value any, path []any, frame int) error {
		if frame > recursionLimit {
			return recursionError()
		}
		child := func(key any) []any { return append(append(make([]any, 0, len(path)+1), path...), key) }
		switch value := value.(type) {
		case *json.Object:
			if value.Len() > 0 {
				for _, key := range value.Keys() {
					if err := visit(value.Value(key), child(key), frame+1); err != nil {
						return err
					}
				}
				return nil
			}
		case []any:
			if len(value) > 0 {
				for index, item := range value {
					if err := visit(item, child(json.IntOf(int64(index))), frame+1); err != nil {
						return err
					}
				}
				return nil
			}
		case string:
			if frame+2 > recursionLimit { // json.dumps and JSONEncoder.encode
				return recursionError()
			}
			if size, _ := dumpsLen(value); size > policy.ClipChars/2 {
				// JSON may expand each Unicode character to twelve ASCII characters.
				// Limit the encoded size, not just the number of input characters.
				pieces := fragments(value)
				for index, piece := range pieces {
					fields = append(fields, json.NewObject("path", path, "value", piece,
						"string_fragment", json.IntOf(int64(index)), "string_fragment_count", json.IntOf(int64(len(pieces)))))
				}
				return nil
			}
		}
		fields = append(fields, json.NewObject("path", path, "value", value))
		return nil
	}
	if err := visit(toolInput, []any{}, frame+1); err != nil {
		return nil, err
	}
	context := &json.Object{}
	for _, key := range toolInput.Keys() {
		value, ok := toolInput.Value(key).(string)
		if (key == "file_path" || key == "path" || key == "file" || key == "notebook_path") && ok && pystr.Len(value) <= 512 {
			context.Set(key, value)
		}
	}
	template := json.NewObject("operation_context", context, "fields", []any{}, "fragment_index", json.IntOf(0),
		"fragment_count", json.IntOf(0), "input_sha256", sha256Hex(serialized),
		"format", "Complete tool input as field paths and values. Consecutive string fragments keep "+
			"their original field role. old_string is existing content, not a proposed change.")
	withFields := func(part *json.Object, fields []any) *json.Object {
		part = part.Copy()
		part.Set("fields", fields)
		return part
	}
	var parts []*json.Object
	current := withFields(template, []any{})
	for _, field := range fields {
		existing := current.Value("fields").([]any)
		candidate := withFields(current, append(append(make([]any, 0, len(existing)+1), existing...), field))
		size, err := dumpsLen(candidate)
		if err != nil {
			return nil, err
		}
		if size <= policy.ClipChars-2_100 {
			current = candidate
			continue
		}
		if len(existing) > 0 {
			parts = append(parts, current)
		}
		current = withFields(template, []any{field})
		if size, err = dumpsLen(current); err != nil {
			return nil, err
		}
		if size > policy.ClipChars-2_100 {
			return nil, requestError("tool input contains an oversized field path; submit smaller edits")
		}
	}
	parts = append(parts, current)
	result := make([]*json.Object, len(parts))
	for index, part := range parts {
		var visible []string
		for _, item := range part.Value("fields").([]any) {
			field := item.(*json.Object)
			value, ok := field.Value("value").(string)
			if !ok || containsValue(field.Value("path").([]any), "old_string") {
				continue
			}
			for _, line := range pystr.SplitLines(value) {
				if pystr.Strip(line) != "" && !hasAnyPrefix(pystr.LStrip(line), "#", "//") {
					visible = append(visible, line)
				}
			}
		}
		// The complete input is still in fields. This derived excerpt directs
		// attention to executable content in otherwise repetitive fragments.
		excerpt := strings.Join(visible, "\n")
		for {
			size, _ := dumpsLen(excerpt)
			if size <= 2_000 {
				break
			}
			excerpt = evidence.Clip(excerpt, max(100, pystr.Len(excerpt)/2))
		}
		part = part.Copy()
		part.Set("fragment_index", json.IntOf(int64(index)))
		part.Set("fragment_count", json.IntOf(int64(len(parts))))
		part.Set("proposed_excerpt", excerpt)
		result[index] = part
	}
	return result, nil
}

// containsValue reports `s in path` for a field path of str keys and int indexes.
func containsValue(path []any, s string) bool {
	for _, item := range path {
		if is(item, s) {
			return true
		}
	}
	return false
}

// future is a concurrent.futures.Future that one goroutine completes.
type future struct {
	done  chan struct{}
	value any
	err   error
}

// askParts asks about each part in parallel within one review timeout.
//
// py: agent_decisions/gates.py _ask_parts
func askParts(ctx Context, states []*json.Object, questions *json.Object, ask Ask) ([]any, error) {
	if len(states) == 1 {
		cached, err := cachedAnswers(ctx, "edit", states[0])
		if err != nil {
			return nil, err
		}
		if json.Truthy(cached) {
			return []any{cached}, nil
		}
		answers, err := ask(states[0], questions)
		if err != nil {
			return nil, err
		}
		return []any{answers}, nil
	}
	deadline := now().Add(reviewer(ctx).Timeout)
	futures := make([]*future, len(states))
	for i, state := range states {
		f := &future{done: make(chan struct{})}
		futures[i] = f
		cached, err := cachedAnswers(ctx, "edit", state)
		if err != nil {
			return nil, err
		}
		if cached != nil {
			f.value = cached
			close(f.done)
			continue
		}
		go func(part *json.Object) {
			defer close(f.done)
			answers, err := ask(part, questions)
			f.value, f.err = answers, err
		}(state)
	}
	results := make([]any, len(futures))
	for i, f := range futures {
		select {
		case <-f.done:
		default:
			timer := time.NewTimer(max(0, deadline.Sub(now())))
			select {
			case <-f.done:
				timer.Stop()
			case <-timer.C:
				return nil, &jev.JevError{Reason: "timeout"}
			}
		}
		if f.err != nil {
			return nil, f.err
		}
		results[i] = f.value
	}
	return results, nil
}

var interpreters = map[string]bool{"sh": true, "bash": true, "zsh": true, "python": true, "python3": true,
	"node": true, "ruby": true, "lua": true, "php": true, "sqlite3": true, "psql": true}

var stdinRedirects = map[string]bool{"<": true, "<<": true, "<<-": true, "<<<": true}

var replModules = map[string]bool{"code": true, "pdb": true, "asyncio": true, "IPython": true, "bpython": true,
	"ptpython": true}

// shellStdinFlags is the bash flag pattern -[a-z]*[si][a-z]* for fullmatch; the classes are ASCII.
var shellStdinFlags = regexp.MustCompile(`^-[a-z]*[si][a-z]*$`)

// interactive reports whether command starts an interpreter that reads code from later stdin. The error is
// the ValueError of the non-POSIX lexer, which Python does not catch.
//
// py: agent_decisions/gates.py _interactive
func interactive(command string) (bool, error) {
	argv, err := shlex.Split(command)
	if err != nil || len(argv) == 0 {
		return false, nil
	}
	name := pathlib.Name(pathlib.Str(argv[0]))
	if !interpreters[name] {
		return false, nil
	}
	redirects := shlex.New(pystr.SplitLines(command)[0], false, shlex.DefaultPunctuation)
	redirects.WhitespaceSplit = true
	suppliedStdin := false
	for !suppliedStdin {
		token, ok, err := redirects.GetToken()
		if err != nil {
			return false, err
		}
		if !ok {
			break
		}
		suppliedStdin = stdinRedirects[token]
	}
	if len(argv) == 1 {
		return true, nil
	}
	for index, arg := range argv[1:] {
		index++
		switch {
		case strings.HasPrefix(name, "python") && strings.HasPrefix(arg, "-m"):
			module := arg[2:]
			if arg == "-m" && index+1 < len(argv) {
				module = argv[index+1]
			}
			return replModules[module], nil
		case arg == "-c" || arg == "--eval" || arg == "-e":
			return false, nil // the code or module is already in this tool call
		case arg == "-i" || arg == "--interactive":
			return true, nil
		case arg == "-":
			return !suppliedStdin, nil
		case (name == "sh" || name == "bash" || name == "zsh") && shellStdinFlags.MatchString(arg):
			return strings.Contains(arg, "i") || !suppliedStdin, nil
		case !strings.HasPrefix(arg, "-"):
			return false, nil // a script filename, rather than a stdin program
		}
	}
	return false, nil
}

// eligibleDecisions returns the consulted advisory and binding verdicts that still apply to subject. frame
// is _eligible_decisions's Python frame.
//
// py: agent_decisions/gates.py _eligible_decisions
func eligibleDecisions(ctx Context, subject string, frame int) ([]*json.Object, error) {
	entries, err := Consulted(ctx)
	if err != nil {
		return nil, err
	}
	var e expr
	eligible := []*json.Object{}
	for _, entry := range entries {
		if !e.in(e.item(entry, "action"), map[string]bool{"advisory": true, "binding": true}) {
			if e.err != nil {
				return nil, e.err
			}
			continue
		}
		contracts := e.items(e.get(entry, "contracts", &json.Object{}))
		changed := false
		for _, path := range contracts.Keys() {
			observed := e.get(e.get(e.get(ctx.Evidence, "contract_observations", &json.Object{}), path, &json.Object{}), "sha256", nil)
			if !e.equal(observed, contracts.Value(path)) {
				changed = true
				break
			}
		}
		if e.err != nil {
			return nil, e.err
		}
		if changed {
			continue
		}
		scope := e.get(e.get(entry, "state", &json.Object{}), "scope", &json.Object{})
		scoped := json.Truthy(e.get(scope, "paths", nil))
		if e.err != nil {
			return nil, e.err
		}
		if scoped {
			root := e.get(e.get(ctx.Evidence, "metadata", &json.Object{}), "source_root", nil)
			if e.err != nil {
				return nil, e.err
			}
			matches, err := scopeMatches(scope.(*json.Object), subject, root, frame+1)
			if err != nil {
				return nil, err
			}
			if !matches {
				continue
			}
			stale := false
			sources := e.get(e.get(e.get(entry, "state", &json.Object{}), "evidence", &json.Object{}), "source_files", []any{})
			for _, item := range e.iter(sources) {
				if !is(e.get(item, "trust", nil), "verified_file_observation") {
					continue
				}
				observed := e.get(e.get(e.get(ctx.Evidence, "file_observations", &json.Object{}), e.item(item, "path"), &json.Object{}), "sha256", nil)
				if !e.equal(observed, e.item(item, "sha256")) {
					stale = true
					break
				}
			}
			if e.err != nil {
				return nil, e.err
			}
			if stale {
				continue
			}
		}
		eligible = append(eligible, entry)
	}
	return eligible, nil
}

// decisionSummaries returns the eligible decisions most relevant to subject that fit a clip. frame is
// _decision_summaries's Python frame.
//
// py: agent_decisions/gates.py _decision_summaries
func decisionSummaries(ctx Context, subject string, frame int) ([]any, error) {
	terms := evidence.Terms(subject)
	entries, err := eligibleDecisions(ctx, subject, frame+1)
	if err != nil {
		return nil, err
	}
	type ranking struct {
		score, index int
		summary      *json.Object
	}
	var e expr
	var ranked []ranking
	for index, entry := range entries {
		options := e.get(e.get(entry, "state", &json.Object{}), "options", &json.Object{})
		summary := json.NewObject("decision", e.get(entry, "decision", ""), "pick", e.get(entry, "pick", ""),
			"selected_approach", e.get(options, e.get(entry, "pick", nil), ""),
			"options", options, "action", e.item(entry, "action"))
		if json.Truthy(e.get(entry, "receipt_id", nil)) {
			summary.Set("receipt_id", e.item(entry, "receipt_id"))
			summary.Set("scope", e.get(e.get(entry, "state", &json.Object{}), "scope", &json.Object{}))
			summary.Set("origin", e.get(entry, "origin", "own_consultation"))
		}
		if e.err != nil {
			return nil, e.err
		}
		text, err := json.Dumps(summary, json.Options{})
		if err != nil {
			return nil, err
		}
		score := 0
		for term := range evidence.Terms(text) {
			if terms[term] {
				score++
			}
		}
		ranked = append(ranked, ranking{score, index, summary})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].index > ranked[j].index
	})
	selected := []any{}
	for _, item := range ranked {
		size, err := dumpsLen(append(append(make([]any, 0, len(selected)+1), selected...), item.summary))
		if err != nil {
			return nil, err
		}
		if size <= policy.ClipChars {
			selected = append(selected, item.summary)
		}
	}
	return selected, nil
}

// scopeMatches reports whether every target path in subject lies inside scope.paths. frame is
// _scope_matches's Python frame.
//
// py: agent_decisions/gates.py _scope_matches
func scopeMatches(scope *json.Object, subject string, root any, frame int) (bool, error) {
	value, err := json.Loads(subject)
	if err != nil {
		if pyerr.IsInstance(err, "ValueError") {
			return true, nil // final prose has no executable target paths
		}
		return false, err
	}
	var paths []string
	var collect func(item any, frame int) error
	collect = func(item any, frame int) error {
		if frame > recursionLimit {
			return recursionError()
		}
		switch item := item.(type) {
		case *json.Object:
			for _, key := range item.Keys() {
				child := item.Value(key)
				text, isText := child.(string)
				switch {
				case isText && (key == "file_path" || key == "path" || key == "file" || key == "notebook_path"):
					paths = append(paths, text)
				case isText && strings.HasPrefix(text, "*** Begin Patch"):
					if frame+2 > recursionLimit { // re.findall and re._compile
						return recursionError()
					}
					paths = append(paths, patchPaths(text)...)
				default:
					if err := collect(child, frame+1); err != nil {
						return err
					}
				}
			}
		case []any:
			for _, child := range item {
				if err := collect(child, frame+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(value, frame+1); err != nil {
		return false, err
	}
	var e expr
	for _, raw := range paths {
		candidate := pathlib.Str(raw)
		if root != nil {
			text, ok := root.(string)
			if !ok {
				return false, &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf(
					"argument should be a str or an os.PathLike object where __fspath__ returns a str, not '%s'", json.TypeName(root))}
			}
			base, err := resolve(text)
			if err != nil {
				return false, err
			}
			resolved, err := resolve(joinPath(base, candidate))
			if err != nil {
				return false, err
			}
			relative, ok := relativeTo(resolved, base)
			if !ok {
				return false, nil
			}
			candidate = relative
		} else if isAbs(candidate) || contains(parts(candidate), "..") {
			return false, nil
		}
		covered := false
		for _, item := range e.iter(e.item(scope, "paths")) {
			allowed, ok := item.(string)
			if !ok {
				e.fail("AttributeError", "'%s' object has no attribute 'rstrip'", json.TypeName(item))
				break
			}
			if candidate == strings.TrimRight(allowed, "/") || strings.HasSuffix(allowed, "/") && strings.HasPrefix(candidate, allowed) {
				covered = true
				break
			}
		}
		if e.err != nil {
			return false, e.err
		}
		if !covered {
			return false, nil
		}
	}
	return true, nil // unknown shell targets still require the semantic coverage check
}

// EditGate has Jev judge each edit, including shell writes, against this agent's completed decisions.
// frame is edit_gate's Python frame (7 under hook.py).
//
// py: agent_decisions/gates.py edit_gate
func EditGate(name string, toolInput *json.Object, ctx Context, ask Ask, frame int) (Outcome, error) {
	subject, err := json.Dumps(toolInput, json.Options{})
	if err != nil {
		return Outcome{}, err
	}
	decisions, err := decisionSummaries(ctx, subject, frame+1)
	if err != nil {
		return Outcome{}, err
	}
	parts, err := pendingParts(toolInput, frame+1)
	if err != nil {
		if pyerr.IsInstance(err, "RequestError") {
			return Outcome{Action: "deny", Message: err.Error(),
				Record: json.NewObject("gate", "edit", "input_error", err.Error())}, nil
		}
		return Outcome{}, err
	}
	state := json.NewObject("policy", reviewPolicy(ctx), "pending_tool", name, "pending_input", parts[0],
		"decisions", decisions, "evidence", ctx.Evidence)
	questions := json.NewObject(
		"needs_decision", noul(
			"If `pending_input` changes files, does it select a new material engineering, design, or product choice "+
				"that the user has not specified exactly? Judge the actual change against the user's words in "+
				"`evidence`. A general request to fix or build something does not specify its implementation. "+
				"For chunked input, judge only the proposed changes visible in this fragment. Comments, padding, "+
				"and existing old_string content alone select no approach. `proposed_excerpt` highlights visible "+
				"non-comment lines; fields retains all input. Executable statements that choose a storage backend, "+
				"library, algorithm, or architecture count even when surrounded by comments. Other fragments "+
				"are checked separately.",
			"The change selects an implementation, approach, design, or product behaviour.",
			"It applies a settled instruction, runs tests, formats text, records facts, adds regression coverage "+
				"for selected behavior, or corrects a failure with existing helpers or test setup without changing "+
				"the selected approach or interfaces."),
		"covered", noul(
			"Does the implementation choice in `pending_input` match `selected_approach` in `decisions`? "+
				"The hook has verified that these are completed "+reviewerName(ctx)+" verdicts. Match the approach by meaning, "+
				"not exact source syntax. Respect explicit `scope.paths` and `scope.invariants`. "+
				"Computed shell targets outside the scope are not covered. One decision can cover several edits implementing its "+
				"approach. Do not judge whether the code is correct or fully tested. A documented evidence-based "+
				"override of an advisory pick is also covered. An unrelated decision or an empty list is not.",
			"The change implements an approach selected in a completed decision, or an evidence-based advisory override.",
			"The change selects a different approach, or no completed decision addresses that approach."),
	)
	if !policy.EditTools[name] {
		questions.Set("writes_files", noul(
			"Does `pending_tool` with `pending_input` create, edit, delete, or replace files or document "+
				"contents? Include shell redirects, scripts, patch commands, MCP file APIs, generated documents, "+
				"and indirect writes. Routine logging and internal caches do not count as an implementation change.",
			"The requested operation changes file or document contents.",
			"The requested operation only reads data, prints output, or waits for existing work."))
	}
	if policy.ShellTools[name] {
		questions.Set("accepts_code_input", noul(
			"Does this command execute commands, source code, or SQL supplied by later stdin? "+
				"Distinguish executable input from ordinary data, test progress, or a signal used to stop a process.",
			"Future stdin can execute commands, code, or database mutations that are absent from this tool input.",
			"The operation is fully specified now, or future stdin only carries data or control signals."))
	}
	contract, err := cacheContract(ctx)
	if err != nil {
		return Outcome{}, err
	}
	record := json.NewObject("gate", "edit", "state", state, "cache_contract", contract)
	states := make([]*json.Object, len(parts))
	for i, part := range parts {
		states[i] = state.Copy()
		states[i].Set("pending_input", part)
	}
	record.Set("state", states[0])
	results, err := askParts(ctx, states, questions, ask)
	if err != nil {
		var failure *jev.JevError
		if !errors.As(err, &failure) {
			return Outcome{}, err
		}
		open, err := ledger.FailOpen(ctx.Host)
		if err != nil {
			return Outcome{}, err
		}
		failed := record.Copy()
		failed.Set("error", failure.Reason)
		if !open {
			return Outcome{Action: "deny", Message: "Required review unavailable (" + reviewer(ctx).Name + " " + failure.Error() + "); action not run. " +
				"Restore the review service and retry.", Record: failed}, nil
		}
		return Outcome{Action: "allow", Record: failed,
			Note: "agent-decisions: edit check skipped (" + reviewer(ctx).Name + " " + failure.Error() + "); action allowed"}, nil
	}
	var e expr
	noulOf := func(answer any, name string) any { return e.item(e.item(answer, name), "noul") }
	writes := policy.EditTools[name]
	for _, answer := range results {
		if writes {
			break
		}
		writes = e.ge(noulOf(answer, "writes_files"), stopAt)
	}
	coverageAt := bindAt
	if ctx.AgentID == nil {
		coverageAt = followAt
	}
	uncovered := func(answer any) bool {
		return e.ge(noulOf(answer, "needs_decision"), stopAt) &&
			(len(decisions) == 0 || e.lt(noulOf(answer, "covered"), coverageAt))
	}
	deny := false
	for _, answer := range results {
		if !writes || deny {
			break
		}
		deny = uncovered(answer)
	}
	opaque := false
	if policy.ShellTools[name] && e.err == nil {
		if opaque, err = interactive(CommandOf(toolInput)); err != nil {
			return Outcome{}, err
		}
		for _, answer := range results {
			if opaque {
				break
			}
			opaque = e.ge(noulOf(answer, "accepts_code_input"), stopAt)
		}
	}
	var failed []string
	for _, answer := range results {
		if uncovered(answer) {
			failed = append(failed, "needs_decision="+e.fixed2(noulOf(answer, "needs_decision"))+" (at least 0.70), "+
				"covered="+e.fixed2(noulOf(answer, "covered"))+" (requires "+formatFixed2(coverageAt)+")")
		}
	}
	if e.err != nil {
		return Outcome{}, e.err
	}
	message := ""
	if deny {
		message = "Edit not run: this change requires a " + reviewerName(ctx) + " decision. " +
			"Eligible receipts: " + strconv.Itoa(len(decisions)) + ". " + strings.Join(failed, "; ") + ". " +
			"Run ts-decide on the choice with neutral options and captured user excerpts. " +
			"Read the verdict before retrying this edit."
	}
	if opaque {
		deny = true
		message = "Submit the complete commands or code in a new shell call. This host does not hook later " +
			"stdin, so an interactive code reader cannot authorize file changes through stdin."
	}
	audit := record.Copy()
	if len(results) == 1 {
		audit.Set("answers", results[0])
	} else {
		chunks := make([]any, len(results))
		for i, answer := range results {
			chunks[i] = json.NewObject("state", states[i], "answers", answer)
		}
		audit.Set("chunks", chunks)
	}
	action := "allow"
	if deny {
		action = "deny"
	}
	return Outcome{Action: action, Message: message, Record: audit}, nil
}

// QuestionGate keeps the agent from asking the user what the user already answered or what the agent
// should decide.
//
// py: agent_decisions/gates.py question_gate
func QuestionGate(toolInput *json.Object, ctx Context, ask Ask) (Outcome, error) {
	var e expr
	or := func(values ...any) any {
		for _, value := range values[:len(values)-1] {
			if json.Truthy(value) {
				return value
			}
		}
		return values[len(values)-1]
	}
	asked := []any{}
	var texts []any
	for _, item := range e.iter(or(toolInput.Value("questions"), []any{})) {
		q, ok := item.(*json.Object)
		if !ok {
			continue
		}
		question := display(or(q.Value("question"), q.Value("title"), ""))
		options := []any{}
		for _, option := range e.iter(or(q.Value("options"), []any{})) {
			switch option := option.(type) {
			case *json.Object:
				label, ok := option.Get("label")
				if !ok {
					label = ""
				}
				options = append(options, display(label))
			case string:
				options = append(options, option)
			}
		}
		asked = append(asked, json.NewObject("question", question, "options", options))
		texts = append(texts, question)
	}
	if e.err != nil {
		return Outcome{}, e.err
	}
	if len(asked) == 0 {
		return Outcome{Action: "allow"}, nil
	}
	size, err := dumpsLen(asked)
	if err != nil {
		return Outcome{}, err
	}
	if len(asked) > 16 || size > policy.RequestChars {
		return Outcome{Action: "deny", Message: "Question request exceeds its budget. Submit at most 16 concise questions " +
			"within 16,000 JSON characters."}, nil
	}
	encoded, err := json.Dumps(texts, json.Options{})
	if err != nil {
		return Outcome{}, err
	}
	key := sha256Hex(encoded)[:16]
	state := json.NewObject("policy", reviewPolicy(ctx), "questions", asked, "evidence", ctx.Evidence)
	questions := &json.Object{}
	for i := range asked {
		questions.Set(fmt.Sprintf("stated_%d", i), noul(
			fmt.Sprintf("Do the user's messages in `evidence` already state the answer to `questions[%d].question`?", i),
			"The user's own words already give the answer.", "The user has not answered this."))
		questions.Set(fmt.Sprintf("owner_%d", i), ownerQuestion(fmt.Sprintf("`questions[%d].question`", i)))
	}
	answers, failure, err := review(ctx, "question", state, questions, ask)
	if err != nil {
		return Outcome{}, err
	}
	if failure != nil {
		open, err := ledger.FailOpen(ctx.Host)
		if err != nil {
			return Outcome{}, err
		}
		if !open {
			return Outcome{Action: "deny", Message: "Required question review unavailable (" + reviewer(ctx).Name + " " + failure.Error() + "); question not sent."}, nil
		}
		return Outcome{Action: "allow", Note: "agent-decisions: question gate skipped (" + reviewer(ctx).Name + " " + failure.Error() + ")"}, nil
	}
	var problems []string
	for i, text := range texts {
		quoted := pystr.Repr(text.(string))
		if e.ge(e.item(e.item(answers, fmt.Sprintf("stated_%d", i)), "noul"), bindAt) {
			problems = append(problems, "- "+quoted+": the user's messages already answer this. Use their answer "+
				"and quote it.")
		} else if e.ge(e.get(e.item(e.item(answers, fmt.Sprintf("owner_%d", i)), "probabilities"), "agent", 0.0), bindAt) {
			problems = append(problems, "- "+quoted+": this is your call under the policy. Decide it (ts-decide "+
				"for a choice between approaches) and state the call.")
		}
	}
	if e.err != nil {
		return Outcome{}, e.err
	}
	action, message := "allow", ""
	if len(problems) > 0 {
		action, message = "deny", "Question not sent.\n"+strings.Join(problems, "\n")
	}
	contract, err := cacheContract(ctx)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Action: action, Message: message, Record: json.NewObject("gate", "question", "question_key", key,
		"state", state, "answers", answers, "cache_contract", contract)}, nil
}

// review returns `_cached_answers(ctx, gate, state) or ask(state, questions)`, with a JevError from ask
// as failure.
func review(ctx Context, gate string, state, questions *json.Object, ask Ask) (answers any, failure *jev.JevError, err error) {
	cached, err := cachedAnswers(ctx, gate, state)
	if err != nil || json.Truthy(cached) {
		return cached, nil, err
	}
	fresh, err := ask(state, questions)
	if err != nil {
		if errors.As(err, &failure) {
			return nil, failure, nil
		}
		return nil, nil, err
	}
	return fresh, nil, nil
}

// PlanGate checks a plan before the host submits it. A nil ask only checks for a completed consultation;
// a nil plan is the last assistant text. frame is plan_gate's Python frame (7 under hook.py).
//
// py: agent_decisions/gates.py plan_gate
func PlanGate(ctx Context, ask Ask, plan *string, frame int) (Outcome, error) {
	subject := ctx.Transcript.LastAssistantText
	if plan != nil {
		subject = *plan
	}
	decided, err := eligibleDecisions(ctx, subject, frame+1)
	if err != nil {
		return Outcome{}, err
	}
	if ask == nil {
		allowed := len(decided) > 0
		if !allowed {
			open, err := ledger.FailOpen(ctx.Host)
			if err != nil {
				return Outcome{}, err
			}
			if open {
				entries, err := Consulted(ctx)
				if err != nil {
					return Outcome{}, err
				}
				var e expr
				for _, entry := range entries {
					if allowed = is(e.item(entry, "action"), "unverified"); allowed || e.err != nil {
						break
					}
				}
				if e.err != nil {
					return Outcome{}, e.err
				}
			}
		}
		if allowed {
			return Outcome{Action: "allow"}, nil
		}
		return Outcome{Action: "deny", Message: "Plan not submitted: no completed ts-decide. Consult on the plan's main choice.",
			Record: json.NewObject("gate", "plan")}, nil
	}
	if pystr.Len(subject) > policy.FinalMessageChars {
		return Outcome{Action: "deny", Message: "Plan exceeds 16,000 characters; submit a concise plan."}, nil
	}
	decisions, err := decisionSummaries(ctx, subject, frame+1)
	if err != nil {
		return Outcome{}, err
	}
	state := json.NewObject("policy", reviewPolicy(ctx), "plan", subject, "evidence", ctx.Evidence, "decisions", decisions)
	questions := json.NewObject(
		"needs_decision", noul("Does the plan in `plan` and the recent tool observations select a material "+
			"approach that the user has not already specified?",
			"It selects an implementation, approach, or design.",
			"It only carries out a settled instruction or routine bookkeeping."),
		"covered", noul("Is every material approach in the plan covered by a relevant completed verdict in "+
			"`decisions`? An unrelated consultation does not count.",
			"The relevant completed decisions cover the plan's choices.",
			"At least one choice lacks a relevant completed decision."),
	)
	answers, failure, err := review(ctx, "plan", state, questions, ask)
	if err != nil {
		return Outcome{}, err
	}
	if failure != nil {
		open, err := ledger.FailOpen(ctx.Host)
		if err != nil {
			return Outcome{}, err
		}
		if !open {
			return Outcome{Action: "deny", Message: "Required plan review unavailable (" + reviewer(ctx).Name + " " + failure.Error() + "); plan not submitted."}, nil
		}
		return Outcome{Action: "allow", Note: "agent-decisions: plan check skipped (" + reviewer(ctx).Name + " " + failure.Error() + ")"}, nil
	}
	var e expr
	deny := e.ge(e.item(e.item(answers, "needs_decision"), "noul"), stopAt) &&
		(len(decisions) == 0 || e.lt(e.item(e.item(answers, "covered"), "noul"), bindAt))
	if e.err != nil {
		return Outcome{}, e.err
	}
	contract, err := cacheContract(ctx)
	if err != nil {
		return Outcome{}, err
	}
	action, message := "allow", ""
	if deny {
		action, message = "deny", "Consult on the plan's uncovered choice before submitting it."
	}
	return Outcome{Action: action, Message: message, Record: json.NewObject("gate", "plan", "state", state,
		"answers", answers, "cache_contract", contract)}, nil
}

// removeFences returns re.sub(r"(?ms)^ {0,3}(`{3,}|~{3,})[^\n]*\n.*?^ {0,3}\1[ \t]*(?:\n|$)", "", text).
// A fence opens at a line start after at most three spaces; the longest fence run that has a closing line
// wins, and the body is the shortest that reaches one.
//
// py: agent_decisions/gates.py stop_gate (hand-port: backreference)
func removeFences(text string) string {
	var out strings.Builder
	last := 0
	for start := 0; start < len(text); {
		if end, ok := fenceAt(text, start); ok {
			out.WriteString(text[last:start])
			last, start = end, end
			continue
		}
		newline := strings.IndexByte(text[start:], '\n')
		if newline < 0 {
			break
		}
		start += newline + 1
	}
	out.WriteString(text[last:])
	return out.String()
}

// indentEnd returns the end of up to three spaces at s[i:].
func indentEnd(s string, i int) int {
	for n := 0; n < 3 && i < len(s) && s[i] == ' '; n++ {
		i++
	}
	return i
}

// fenceAt matches a fenced block at line start i and returns its end.
func fenceAt(text string, i int) (int, bool) {
	i = indentEnd(text, i)
	if i >= len(text) || text[i] != '`' && text[i] != '~' {
		return 0, false
	}
	run := i
	for run < len(text) && text[run] == text[i] {
		run++
	}
	newline := strings.IndexByte(text[run:], '\n')
	if newline < 0 {
		return 0, false
	}
	body := run + newline + 1
	for fence := text[i:run]; len(fence) >= 3; fence = fence[:len(fence)-1] {
		for line := body; ; {
			at := indentEnd(text, line)
			if strings.HasPrefix(text[at:], fence) {
				end := blankEnd(text, at+len(fence))
				if end == len(text) {
					return end, true
				}
				if text[end] == '\n' {
					return end + 1, true
				}
			}
			next := strings.IndexByte(text[line:], '\n')
			if next < 0 {
				break
			}
			line += next + 1
		}
	}
	return 0, false
}

// paragraphBreak is \n\s*\n; RE2's leftmost-first match is Python's for this pattern.
var paragraphBreak = regexp.MustCompile(`\n` + pyre.Space + `*\n`)

// StopGate checks the final reply for unreviewed choices, needless questions and ignored binding picks.
// frame is stop_gate's Python frame.
//
// py: agent_decisions/gates.py stop_gate
func StopGate(final string, stopHookActive bool, ctx Context, ask Ask, frame int) (Outcome, error) {
	final = pystr.Strip(final)
	if pystr.Len(final) > policy.FinalMessageChars {
		return Outcome{Action: "block", Message: "Reply exceeds 16,000 characters. Keep the final reply concise " +
			"or save the detailed report in a file, then finish."}, nil
	}
	if final == "" {
		return Outcome{Action: "allow"}, nil
	}
	decided, err := eligibleDecisions(ctx, final, frame+1)
	if err != nil {
		return Outcome{}, err
	}
	var binding *json.Object
	for i := len(decided) - 1; i >= 0; i-- {
		if is(decided[i].Value("action"), "binding") {
			binding = decided[i]
			break
		}
	}
	instruction := ""
	if messages := ctx.Transcript.UserMessages; len(messages) > 0 {
		instruction = evidence.Clip(messages[len(messages)-1], policy.ClipChars)
	}
	state := json.NewObject("policy", reviewPolicy(ctx), "final_message", final,
		"question_message", pystr.Strip(removeFences(final)), "evidence", ctx.Evidence, "gate_contract", "stop-4",
		"current_user_instruction", instruction)
	decisions, err := decisionSummaries(ctx, final, frame+1)
	if err != nil {
		return Outcome{}, err
	}
	state.Set("decisions", decisions)
	paragraphs := paragraphBreak.Split(final, -1)
	if len(paragraphs) > 16 {
		paragraphs = append(paragraphs[:15:15], strings.Join(paragraphs[15:], "\n\n"))
	}
	sections := &json.Object{}
	for index, paragraph := range paragraphs {
		sections.Set("section_"+strconv.Itoa(index), evidence.Clip(paragraph, 1_000))
	}
	state.Set("final_sections", sections)
	criteria := sections.Copy()
	criteria.Set("none_fit", "No section contains an uncovered material choice.")
	questions := json.NewObject(
		"made_choice", noul("Judge only `final_message`: does the agent state that it selected an alternative? "+
			"Include algorithms, libraries, databases, implementations, designs, and root-cause theories. "+
			"Detect the selection itself. Whether the user requested it is a separate question.",
			"The reply identifies a selected alternative or approach.",
			"The reply only reports observations or routine progress, without selecting an alternative."),
		"settled_choice", noul("Do the user's actual words already specify the exact choices stated in `final_message`? "+
			"Read user_messages in chronological order. A later specific instruction supersedes an earlier "+
			"verdict. Semantically identical wording counts; a general goal or instruction to continue does not.",
			"The user explicitly specified that exact choice.",
			"The agent selected the choice, or no exact user instruction settles it."),
		"asks_user", noul("Does the agent directly ask the user to decide or answer something in `question_message`? "+
			"Quoted verdicts, reported questions, logs, and next-action instructions are not requests for an answer.",
			"The agent asks the user for an answer now.", "The agent asks the user nothing."),
		"stated", noul("If `question_message` asks the user something, do the user's messages in `evidence` already "+
			"state the answer?", "The user's own words already answer it.",
			"The user has not answered it, or nothing is asked."),
		"owner", ownerQuestion("the question that `question_message` asks the user, if any"),
		"uncovered_section", json.NewObject("type", "choice", "instructions",
			"If a material choice lacks coverage, which section in final_sections contains that choice? "+
				"Compare decisions and captured user instructions. Reporting observations or a completed "+
				"selected approach is not an additional choice.",
			"criteria", criteria),
	)
	if len(decisions) > 0 {
		questions.Set("uncovered_choice", noul(
			"Does `final_message` select any additional or different approach that is absent from "+
				"`selected_approach` in `decisions` and not specifically instructed by the user? Match approaches "+
				"by meaning. A report that the selected approaches were implemented introduces no additional "+
				"choice. An advisory override counts when supported by evidence. A later specific user "+
				"instruction takes precedence.",
			"The reply introduces at least one approach without a relevant consultation or specific user instruction.",
			"The reply only follows selected approaches or specific user instructions, or reports observations."))
	}
	if binding != nil {
		picks := []any{}
		for _, item := range decisions {
			if is(item.(*json.Object).Value("action"), "binding") {
				picks = append(picks, item)
			}
		}
		state.Set("binding_picks", picks)
		questions.Set("follows_pick", noul(
			"Does `final_message` comply with every relevant, unsuperseded decision in `binding_picks`? "+
				"An unmentioned decision is not a violation. A later explicit user instruction takes precedence.",
			"The reply does not contradict any binding choice that still applies.",
			"The reply contradicts at least one binding choice that still applies."))
	}
	answers, failure, err := review(ctx, "stop", state, questions, ask)
	if err != nil {
		return Outcome{}, err
	}
	if failure != nil {
		open, err := ledger.FailOpen(ctx.Host)
		if err != nil {
			return Outcome{}, err
		}
		if !open {
			return Outcome{Action: "block", Message: "Required final review unavailable (" + reviewer(ctx).Name + " " + failure.Error() + "). " +
				"Restore the review service and retry."}, nil
		}
		return Outcome{Action: "allow", Note: "agent-decisions: stop check skipped (" + reviewer(ctx).Name + " " + failure.Error() + ")"}, nil
	}
	var e expr
	noulOf := func(name string) any { return e.item(e.item(answers, name), "noul") }
	var reasons []string
	if e.ge(noulOf("made_choice"), stopAt) && e.lt(noulOf("settled_choice"), bindAt) &&
		(len(decisions) == 0 || e.gt(noulOf("uncovered_choice"), unbound)) {
		section := e.item(e.item(answers, "uncovered_section"), "choice")
		excerpt := e.get(sections, section, "No specific section was identified.")
		probability := e.get(e.get(answers, "uncovered_choice", &json.Object{}), "noul", nil)
		detail := "No verified completed decision covers this reply."
		if probability != nil {
			detail = "uncovered_choice=" + e.fixed2(probability) + " (must be at most " + formatFixed2(1.0-bindAt) + ")"
		}
		if e.err == nil {
			reasons = append(reasons, "You chose between approaches without a relevant ts-decide verdict. "+
				"Eligible receipts: "+strconv.Itoa(len(decisions))+". "+detail+". "+
				"Disputed section "+display(section)+": "+evidence.Clip(excerpt.(string), 500)+"\n"+
				"Consult on that choice, read the verdict, then finish.")
		}
	}
	if e.ge(noulOf("asks_user"), stopAt) {
		if e.ge(noulOf("stated"), bindAt) {
			reasons = append(reasons, "Your reply asks the user something their messages already answer. Use their answer, "+
				"quote it, and continue.")
		} else if e.ge(e.get(e.item(e.item(answers, "owner"), "probabilities"), "agent", 0.0), bindAt) {
			reasons = append(reasons, "Your reply asks the user to decide something that is yours to decide. Decide it "+
				"(ts-decide for a choice between approaches), state the call, and continue.")
		}
	}
	if binding != nil && e.lt(noulOf("settled_choice"), bindAt) && e.lt(noulOf("follows_pick"), followAt) {
		reasons = append(reasons, "The binding pick for "+json.Repr(e.get(binding, "decision", ""))+" is `"+
			display(e.get(binding, "pick", ""))+"`. Your reply does not follow it. Follow it.")
	}
	if e.err != nil {
		return Outcome{}, e.err
	}
	contract, err := cacheContract(ctx)
	if err != nil {
		return Outcome{}, err
	}
	action := "allow"
	if len(reasons) > 0 {
		action = "block"
	}
	return Outcome{Action: action, Message: strings.Join(reasons, "\n"), Record: json.NewObject("gate", "stop",
		"state", state, "answers", answers, "cache_contract", contract)}, nil
}
