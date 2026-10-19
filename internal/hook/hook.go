// Package hook is the hook entry point: one payload in, one response under the protected review policy.
//
// py: agent_decisions/hook.py
package hook

import (
	stdcontext "context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/argparse"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	pyre "github.com/rexbrahh/make-better-decisions/internal/py/re"
	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"github.com/rexbrahh/make-better-decisions/internal/subq"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

// Side effects that tests replace, as the Python tests patch uuid, time and __file__.
var (
	// newID returns uuid.uuid4().hex.
	newID = func() string {
		var b [16]byte
		if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
			panic(err) // crypto/rand does not fail on supported platforms
		}
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		return hex.EncodeToString(b[:])
	}
	now = time.Now
	// perfCounter returns time.perf_counter() in seconds.
	perfCounter = func() float64 { return float64(time.Since(started)) / float64(time.Second) }
	// moduleFile returns the file whose grandparent directory is the code root. Python used
	// <root>/agent_decisions/hook.py; the Go binary is <root>/bin/agent-decisions, at the same depth.
	moduleFile = os.Executable
)

var started = time.Now()

// askMu serializes the writes that concurrent chunk asks make to shared values (the reviewed state and
// metrics["api_calls"]) with the hook's reads of those values when it records an outcome. A chunk that the
// gate stopped waiting for still finishes its ask, as Python's daemon threads do.
var askMu sync.Mutex

// The Python frames of the gate calls under hooks/hook.py, counting <module> as 1.
const (
	guardFrame   = 5 // <module>, main, handle, _pre_tool_use, guard
	gateFrame    = 7 // ... _pre_tool_use, _gate, the lambda, edit_gate or plan_gate
	contextFrame = 6 // ... _pre_tool_use, _gate or _decide, _context
	decideAsk    = 7 // ... _decide, gates.decide, ask
	planAsk      = 8 // ... plan_gate, ask
	editAsk      = 9 // ... edit_gate, _ask_parts, ask
	stdinFrame   = 3 // <module>, main, transcript.loads
)

// Handle returns the host response to one hook event, or nil for no response.
//
// py: agent_decisions/hook.py handle
func Handle(event string, payload any, host string, post jev.PostFunc) (*json.Object, error) {
	given, ok := payload.(*json.Object)
	if !ok {
		return nil, typeError("'%s' object is not a mapping", json.TypeName(payload))
	}
	call := given.Copy()
	toolUseID := given.Value("tool_use_id")
	if !json.Truthy(toolUseID) {
		toolUseID = "local-" + newID()
	}
	call.Set("tool_use_id", toolUseID)
	switch event {
	case "session_start":
		if err := ledger.Prune(); err != nil {
			return nil, err
		}
		if !json.Truthy(call.Value("agent_id")) && json.Truthy(call.Value("transcript_path")) {
			if err := capture(call, host); err != nil && !isOSError(err) {
				return nil, err // an empty startup transcript is normal
			}
		}
		root, err := codeRoot()
		if err != nil {
			return nil, err
		}
		provider, err := ledger.ReviewProvider(host)
		if err != nil {
			return nil, err
		}
		return json.NewObject("hookSpecificOutput", json.NewObject("hookEventName", "SessionStart",
			"additionalContext", "Use the deciding skill before choosing an approach or asking the user to decide. "+
				"Routine checks, exact user instructions and independently authorized factual reads need no approach consultation. "+
				reviewLabel(provider)+" reviews declared edit tools and plan acceptance. Ordinary tools use normal host controls. "+
				"For a material choice, run "+shlex.Quote(pathlib.Join(root, "bin/ts-decide"))+" as a separate shell call. "+
				"Read "+pathlib.Join(root, "skills/deciding/SKILL.md")+" for the request format. "+
				"Read the verdict before executing a dependent call.")), nil
	case "user_prompt_submit":
		prompt, ok := call.Value("prompt").(string)
		if ok && prompt != "" && !json.Truthy(call.Value("agent_id")) {
			outcome := gates.Outcome{Action: "observed", Record: json.NewObject("gate", "user_prompt", "prompt", prompt)}
			return nil, appendRecord(call, host, nil, outcome, nil, 4) // handle, _append
		}
		return nil, nil
	case "pre_tool_use":
		return preToolUse(call, host, post)
	case "stop", "subagent_stop":
		return nil, nil // old loaded registrations must not block ordinary completion
	}
	return nil, &pyerr.Error{Class: "ValueError", Msg: "unknown event " + pystr.Repr(event)}
}

// capture saves the user's words of a new session: the session_start body of Python's try statement.
func capture(payload *json.Object, host string) error {
	t, err := loadTranscript(payload, host)
	if err != nil {
		return err
	}
	entries, err := sessionEntries(orUnknown(payload.Value("session_id")))
	if err != nil {
		return err
	}
	_, err = savedUsers(payload, host, t, entries, 4)
	return err
}

// codeRoot returns Path(__file__).resolve().parent.parent.
func codeRoot() (string, error) {
	file, err := moduleFile()
	if err != nil {
		return "", pyerr.NewOSError(err)
	}
	resolved, err := ospath.Realpath(pathlib.Str(file), false)
	if err != nil {
		return "", err
	}
	return pathlib.Parent(pathlib.Parent(resolved)), nil
}

// py: agent_decisions/hook.py _pre_tool_use
func preToolUse(payload *json.Object, host string, post jev.PostFunc) (*json.Object, error) {
	nameValue := payload.Value("tool_name")
	if !json.Truthy(nameValue) {
		nameValue = ""
	}
	if err := json.Unhashable(nameValue, "set element"); err != nil {
		return nil, err
	}
	name, _ := nameValue.(string) // another hashable value is in no tool set
	toolInput, ok := payload.Value("tool_input").(*json.Object)
	if !ok {
		toolInput = json.NewObject("input", payload.Value("tool_input"))
	}
	if policy.ShellTools[name] {
		kind, request := gates.Classify(gates.CommandOf(toolInput))
		switch kind {
		case "decide":
			return decide(payload, host, toolInput, request, post)
		case "misuse":
			return deny(gates.Misuse), nil
		}
	}
	outcome, err := gates.Guard(toolInput, payload.Value("cwd"), guardFrame)
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		// The denial must not depend on the ledger being writable.
		if err := appendRecord(payload, host, nil, *outcome, nil, 5); err != nil && !isOSError(err) {
			return nil, err
		}
		return deny(outcome.Message), nil
	}
	if host == "claude" && name == "ExitPlanMode" {
		var plan *string
		if text, ok := toolInput.Value("plan").(string); ok {
			plan = &text
		}
		return gate(payload, host, func(ctx gates.Context) (gates.Outcome, error) {
			ask, cancel := reviewAsker(ctx, post, planAsk)
			defer cancel()
			return gates.PlanGate(ctx, ask, plan, gateFrame)
		})
	}
	if policy.EditTools[name] {
		return gate(payload, host, func(ctx gates.Context) (gates.Outcome, error) {
			ask, cancel := reviewAsker(ctx, post, editAsk)
			defer cancel()
			return gates.EditGate(name, toolInput, ctx, ask, gateFrame)
		})
	}
	return nil, nil
}

// py: agent_decisions/hook.py _decide
func decide(payload *json.Object, host string, toolInput *json.Object, request string,
	post jev.PostFunc) (*json.Object, error) {
	contracts, err := requestContracts(request)
	if err != nil {
		return nil, err
	}
	var outcome gates.Outcome
	var turn any
	ctx, err := context(payload, host, contracts)
	switch {
	case isOSError(err):
		ctx = nil
		availability, err2 := ledger.FailOpen(host)
		if err2 != nil {
			return nil, err2
		}
		next := "Required review is unavailable. Restore readable host context and consult again."
		if availability {
			next = "Decide yourself and state your evidence."
		}
		outcome = gates.Outcome{Action: "unverified",
			Message: "ts-decide: decision unverified (transcript unreadable: " + pyClass(err) + "). " + next,
			Record:  json.NewObject("gate", "decide")}
	case err != nil:
		return nil, err
	default:
		turn = ctx.Transcript.TurnKey()
		completed := false
		for _, call := range ctx.Transcript.ToolCalls {
			completed = completed || ctx.ToolUseID == any(call.ID) && call.Output != ""
		}
		for _, entry := range ctx.Entries {
			completed = completed || entry.Value("gate") == "decide" && sameValue(entry.Value("tool_use_id"), ctx.ToolUseID)
		}
		if completed {
			return deny("ts-decide: this tool call has already completed. Run a new, separate ts-decide call."), nil
		}
		ask, cancel := reviewAsker(*ctx, post, decideAsk)
		defer cancel()
		if outcome, err = gates.Decide(request, *ctx, ask); err != nil {
			return nil, err
		}
	}
	keys, err := secrets(host)
	if err != nil {
		return nil, err
	}
	outcome.Message = evidence.Redact(outcome.Message, keys)
	if outcome.Record != nil {
		outcome.Record = outcome.Record.Copy()
		outcome.Record.Set("verdict", outcome.Message)
		if err := appendRecord(payload, host, turn, outcome, ctx, 6); err != nil {
			return nil, err
		}
	}
	if outcome.Action == "invalid" {
		return deny(outcome.Message), nil
	}
	updated := toolInput.Copy()
	for _, key := range []string{"command", "cmd"} {
		if _, ok := updated.Get(key); ok {
			updated.Set(key, "printf '%s\\n' "+shlex.Quote(outcome.Message))
		}
	}
	return json.NewObject("hookSpecificOutput", json.NewObject(
		"hookEventName", "PreToolUse", "permissionDecision", "allow",
		"permissionDecisionReason", "ts-decide verdict",
		"updatedInput", updated)), nil
}

// requestContracts returns the first eight valid scope paths of the request's "contracts", or none when
// they are not valid: gates.Decide retains the existing invalid-request diagnostic.
func requestContracts(request string) ([]string, error) {
	contracts, err := func() ([]string, error) {
		decoded, err := json.Loads(request)
		if err != nil {
			return nil, err
		}
		paths, err := get(decoded, "contracts", []any{})
		if err != nil {
			return nil, err
		}
		raw, err := json.Dumps(json.NewObject("scope", json.NewObject("paths", paths)), json.Options{})
		if err != nil {
			return nil, err
		}
		scope, err := gates.RequestScope(raw)
		if err != nil {
			return nil, err
		}
		values, _ := scope.Value("paths").([]any) // RequestScope checked a list of str
		var contracts []string
		for _, value := range values[:min(8, len(values))] {
			contracts = append(contracts, value.(string))
		}
		return contracts, nil
	}()
	if pyerr.IsInstance(err, "ValueError", "AttributeError", "RequestError") {
		return nil, nil
	}
	return contracts, err
}

// py: agent_decisions/hook.py _gate
func gate(payload *json.Object, host string, run func(gates.Context) (gates.Outcome, error)) (*json.Object, error) {
	ctx, err := context(payload, host, nil)
	if isOSError(err) {
		availability, err2 := ledger.FailOpen(host)
		if err2 != nil {
			return nil, err2
		}
		if !availability {
			return deny("Required review unavailable: transcript unreadable (" + pyClass(err) + "); " +
				"action not run. Restore readable host context and retry."), nil
		}
		return note("agent-decisions: gate skipped, transcript unreadable (" + pyClass(err) + ")"), nil
	}
	if err != nil {
		return nil, err
	}
	if host == "codex" && ctx.Transcript.Identity != nil {
		excepted, err := ledger.SessionReviewException(host, ctx.SessionID)
		if err != nil {
			return nil, err
		}
		if excepted {
			outcome := gates.Outcome{Action: "allow",
				Record: json.NewObject("gate", "owner_exception", "native_session_id", ctx.SessionID)}
			return nil, appendRecord(payload, host, ctx.Transcript.TurnKey(), outcome, ctx, 6)
		}
	}
	outcome, err := run(*ctx)
	if err != nil {
		return nil, err
	}
	if outcome.Record != nil {
		if err := appendRecord(payload, host, ctx.Transcript.TurnKey(), outcome, ctx, 6); err != nil {
			return nil, err
		}
	}
	if outcome.Action == "deny" {
		return deny(outcome.Message), nil
	}
	if outcome.Note != "" {
		return note(outcome.Note), nil
	}
	return nil, nil
}

// py: agent_decisions/hook.py _load_transcript
func loadTranscript(payload *json.Object, host string) (transcript.Transcript, error) {
	mainPath := payload.Value("transcript_path")
	if !json.Truthy(mainPath) {
		mainPath = ""
	}
	ownPath := payload.Value("agent_transcript_path")
	agentID := payload.Value("agent_id")
	if !json.Truthy(ownPath) && json.Truthy(agentID) && json.Truthy(mainPath) {
		base, err := pathArgument(mainPath)
		if err != nil {
			return transcript.Transcript{}, err
		}
		if base, err = withoutSuffix(base); err != nil {
			return transcript.Transcript{}, err
		}
		name, err := pyStr(agentID)
		if err != nil {
			return transcript.Transcript{}, err
		}
		guess := pathlib.Str(base + "/subagents/agent-" + name + ".jsonl")
		ownPath = nil
		if exists(guess) {
			ownPath = guess
		}
	}
	if !json.Truthy(ownPath) || sameValue(ownPath, mainPath) {
		return load(mainPath, host)
	}
	own, err := load(ownPath, host)
	if err != nil {
		return transcript.Transcript{}, err
	}
	main, err := load(mainPath, host)
	if err != nil {
		return transcript.Transcript{}, err
	}
	own.UserMessages, own.UserKeys, own.QuestionReplyKeys = main.UserMessages, main.UserKeys, main.QuestionReplyKeys
	return own, nil
}

// load is transcript.load of a payload value, which must be a path string.
func load(path any, host string) (transcript.Transcript, error) {
	text, err := pathArgument(path)
	if err != nil {
		return transcript.Transcript{}, err
	}
	return transcript.Load(text, host)
}

// withoutSuffix returns str(Path(p).with_suffix("")).
//
// py: Lib/pathlib/_local.py PurePath.with_suffix (Python 3.15)
func withoutSuffix(p string) (string, error) {
	p = pathlib.Str(p)
	name := pathlib.Name(p)
	stem := name
	// "." and "/" are ASCII, so byte offsets are exact here.
	if i := strings.LastIndexByte(name, '.'); i != -1 && strings.TrimLeft(name[:i], ".") != "" {
		stem = name[:i]
	}
	if stem == "" {
		return "", &pyerr.Error{Class: "ValueError", Msg: "PosixPath(" + pystr.Repr(p) + ") has an empty name"}
	}
	return pathlib.Join(pathlib.Parent(p), stem), nil
}

// exists ports os.path.exists: false for any OSError or ValueError of os.stat.
func exists(path string) bool {
	encoded, err := pyerr.Fsencode(path)
	if err != nil {
		return false
	}
	_, err = os.Stat(encoded)
	return err == nil
}

// py: agent_decisions/hook.py _context
func context(payload *json.Object, host string, contracts []string) (*gates.Context, error) {
	start := perfCounter()
	provider, err := ledger.ReviewProvider(host)
	if err != nil {
		return nil, err
	}
	t, err := loadTranscript(payload, host)
	if err != nil {
		return nil, err
	}
	metrics := json.NewObject("transcript_ms", (perfCounter()-start)*1_000)
	canonical := payload.Copy()
	if t.Identity != nil {
		var agentID any
		if t.Identity.AgentID != nil {
			agentID = *t.Identity.AgentID
		}
		canonical.Set("session_id", t.Identity.SessionID)
		canonical.Set("agent_id", agentID)
	}
	keys, err := providerSecrets(provider.Name)
	if err != nil {
		return nil, err
	}
	session := orUnknown(canonical.Value("session_id"))
	ledgerStarted := perfCounter()
	entries, err := sessionEntries(session)
	if err != nil {
		return nil, err
	}
	sessionID := session.(string) // sessionEntries accepts only a str
	if t.UserMessages, err = savedUsers(canonical, host, t, entries, contextFrame+1); err != nil {
		return nil, err
	}
	metrics.Set("ledger_ms", (perfCounter()-ledgerStarted)*1_000)
	cwd, err := payloadCwd(payload)
	if err != nil {
		return nil, err
	}
	repoID, err := repositoryID(cwd)
	if err != nil {
		return nil, err
	}
	if err := recordIdentity(canonical, host, t, entries, repoID); err != nil {
		return nil, err
	}
	evidenceStarted := perfCounter()
	queryValue := payload.Value("tool_input")
	if !json.Truthy(queryValue) {
		queryValue = payload.Value("last_assistant_message")
	}
	if !json.Truthy(queryValue) {
		queryValue = ""
	}
	query, err := json.Dumps(queryValue, json.Options{})
	if err != nil {
		return nil, err
	}
	if cwd, err = payloadCwd(payload); err != nil {
		return nil, err
	}
	bundle, err := evidence.Build(t, keys, evidence.Options{Query: query, Cwd: &cwd, Contracts: contracts,
		Frame: contextFrame + 1, Profile: provider.Evidence})
	if err != nil {
		return nil, err
	}
	metadata := bundle.Value("metadata").(*json.Object) // build always writes it
	metadata.Set("user_history_source", "signed_host_context")
	if cwd, err = payloadCwd(payload); err != nil {
		return nil, err
	}
	sourceRoot, err := resolve(cwd)
	if err != nil {
		return nil, err
	}
	metadata.Set("source_root", sourceRoot)
	if err := evidence.TrimToBudgetWithProfile(bundle, contracts, query, 0, provider.Evidence); err != nil {
		return nil, err
	}
	inherited, err := inheritedDecisions(t, entries, host, sessionID, repoID, keys, canonical, provider)
	if err != nil {
		return nil, err
	}
	ctx := &gates.Context{Host: host, SessionID: sessionID, AgentID: canonical.Value("agent_id"),
		ToolUseID: payload.Value("tool_use_id"), Transcript: t, Evidence: bundle, Entries: entries,
		RepoID: repoID, InheritedEntries: inherited, Metrics: metrics, Reviewer: provider}
	completed, err := gates.Consulted(*ctx)
	if err != nil {
		return nil, err
	}
	if err := ledger.RememberArchitecture(repoID, completed); err != nil {
		return nil, err
	}
	history, err := ledger.ArchitectureEntries(repoID)
	if err != nil {
		return nil, err
	}
	var contractPaths, sourcePaths []string
	for _, entry := range slices.Concat(reversed(completed), reversed(history)) {
		paths, err := get(entry, "contracts", &json.Object{})
		if err != nil {
			return nil, err
		}
		names, err := iterate(paths)
		if err != nil {
			return nil, err
		}
		if contractPaths, err = appendStrings(contractPaths, names); err != nil {
			return nil, err
		}
	}
	for _, entry := range reversed(completed) {
		state, err := get(entry, "state", &json.Object{})
		if err != nil {
			return nil, err
		}
		scope, err := get(state, "scope", &json.Object{})
		if err != nil {
			return nil, err
		}
		scoped, err := get(scope, "paths", nil)
		if err != nil {
			return nil, err
		}
		if !json.Truthy(scoped) {
			continue
		}
		observed, err := get(state, "evidence", &json.Object{})
		if err != nil {
			return nil, err
		}
		files, err := get(observed, "source_files", []any{})
		if err != nil {
			return nil, err
		}
		items, err := iterate(files)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			trust, err := get(item, "trust", nil)
			if err != nil {
				return nil, err
			}
			if trust != "verified_file_observation" {
				continue
			}
			path, err := index(item, "path")
			if err != nil {
				return nil, err
			}
			if sourcePaths, err = appendStrings(sourcePaths, []any{path}); err != nil {
				return nil, err
			}
		}
	}
	currentFiles := &json.Object{}
	sources, _ := bundle.Value("source_files").([]any) // build writes a list of dicts, or nothing
	for _, value := range sources {
		item := value.(*json.Object)
		if item.Value("trust") == "verified_file_observation" {
			currentFiles.Set(item.Value("path").(string),
				json.NewObject("sha256", item.Value("sha256"), "trust", item.Value("trust")))
		}
	}
	var missing []string
	for _, path := range slices.Concat(contractPaths, sourcePaths) {
		if _, ok := currentFiles.Get(path); !ok {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		if cwd, err = payloadCwd(payload); err != nil {
			return nil, err
		}
		observed, err := evidence.ContractObservationsWithProfile(missing, cwd, keys, provider.Evidence)
		if err != nil {
			return nil, err
		}
		for _, path := range observed.Keys() {
			currentFiles.Set(path, observed.Value(path))
		}
	}
	if len(contractPaths) > 0 || len(sourcePaths) > 0 {
		bundle.Set("file_observations", currentFiles)
		contractObservations := &json.Object{}
		for _, path := range contractPaths {
			if observed, ok := currentFiles.Get(path); ok {
				contractObservations.Set(path, observed)
			}
		}
		bundle.Set("contract_observations", contractObservations)
	}
	if len(history) > 0 {
		current, _ := bundle.Value("contract_observations").(*json.Object) // only this function sets it
		if current == nil {
			current = &json.Object{}
		}
		architecture, err := evidence.ArchitectureHistory(history, current)
		if err != nil {
			return nil, err
		}
		bundle.Set("architecture_history", architecture)
	}
	if err := evidence.TrimToBudgetWithProfile(bundle, contracts, query, 0, provider.Evidence); err != nil {
		return nil, err
	}
	metrics.Set("evidence_ms", (perfCounter()-evidenceStarted)*1_000)
	metrics.Set("context_ms", (perfCounter()-start)*1_000)
	return ctx, nil
}

// payloadCwd returns payload.get("cwd") or os.getcwd().
func payloadCwd(payload *json.Object) (string, error) {
	if cwd := payload.Value("cwd"); json.Truthy(cwd) {
		return pathArgument(cwd)
	}
	cwd, err := syscall.Getwd()
	if err != nil {
		return "", pyerr.NewOSError(err)
	}
	return cwd, nil
}

// resolve returns str(Path(p).resolve()).
func resolve(p string) (string, error) {
	return ospath.Realpath(pathlib.Str(p), false)
}

// py: agent_decisions/hook.py _repository_id
func repositoryID(cwd string) (string, error) {
	root, err := resolve(cwd)
	if err != nil {
		return "", err
	}
	timeout, cancel := stdcontext.WithTimeout(stdcontext.Background(), time.Second)
	defer cancel()
	command := exec.CommandContext(timeout, "git", "-C", root, "rev-parse", "--git-common-dir")
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if timeout.Err() != nil {
		return root, nil // subprocess.TimeoutExpired
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return root, nil // an OSError: git is missing or cannot run
	}
	// text=True decodes both streams strictly, with universal newlines.
	for _, data := range []string{stdout.String(), stderr.String()} {
		if !utf8.ValidString(data) {
			return "", &pyerr.Error{Class: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode git output"}
		}
	}
	if command.ProcessState.ExitCode() != 0 {
		return root, nil
	}
	text := strings.ReplaceAll(strings.ReplaceAll(stdout.String(), "\r\n", "\n"), "\r", "\n")
	common := pystr.Strip(text)
	if !strings.HasPrefix(common, "/") {
		common = root + "/" + common
	}
	return resolve(common)
}

// py: agent_decisions/hook.py _record_identity
func recordIdentity(payload *json.Object, host string, t transcript.Transcript, entries []*json.Object,
	repoID string) error {
	identity := t.Identity
	if identity == nil {
		return nil
	}
	pathValue, err := index(payload, "transcript_path")
	if err != nil {
		return err
	}
	path, err := pathArgument(pathValue)
	if err != nil {
		return err
	}
	if path, err = resolve(path); err != nil {
		return err
	}
	record := json.NewObject("gate", "host_identity", "thread_id", identity.ThreadID,
		"transcript_path", path, "repo_id", repoID)
	for _, entry := range entries {
		if entry.Value("gate") != "host_identity" || entry.Value("host") != host {
			continue
		}
		known := true
		for _, key := range record.Keys() {
			known = known && entry.Value(key) == record.Value(key)
		}
		if known {
			return nil
		}
	}
	return appendRecord(payload, host, nil, gates.Outcome{Action: "observed", Record: record}, nil, contextFrame+2)
}

// Patterns of a Claude hand-back footer and of the receipt lines of a dispatch message.
var (
	// [\s\S] is any character, which (?s:.) spells exactly.
	//
	// py: agent_decisions/hook.py _inherited_decisions (?m)^agentId: ([A-Za-z0-9_-]{1,128}) \(use SendMessage[^\n]*\)\n<usage>[\s\S]*</usage>\s*$
	footerPattern = regexp.MustCompile(`(?m)^agentId: ([A-Za-z0-9_-]{1,128}) \(use SendMessage[^\n]*\)\n<usage>(?s:.)*</usage>` +
		pyre.Space + `*$`)
	// py: agent_decisions/hook.py _inherited_decisions (?m)^Accepted option:\s*([A-Za-z0-9_-]{1,40})\s*$
	acceptedPattern = regexp.MustCompile(`(?m)^Accepted option:` + pyre.Space + `*([A-Za-z0-9_-]{1,40})` +
		pyre.Space + `*$`)
	// py: agent_decisions/hook.py _inherited_decisions (?m)^Evidence refs:\s*([^\n]+)$
	referencesPattern = regexp.MustCompile(`(?m)^Evidence refs:` + pyre.Space + `*([^\n]+)$`)
	// py: agent_decisions/hook.py _inherited_decisions (?m)^Decision receipt:\s*([A-Za-z0-9_-]{1,128})\s*$
	receiptPattern = regexp.MustCompile(`(?m)^Decision receipt:` + pyre.Space + `*([A-Za-z0-9_-]{1,128})` +
		pyre.Space + `*$`)
	receiptTaskPattern = regexp.MustCompile(`^ad_(review_[0-9a-f]{32})$`)
)

// assignment is a receipt that a parent dispatch assigned: the accepted option (nil for None) and the
// evidence references.
type assignment struct {
	accepted *string
	refs     []string
}

// py: agent_decisions/hook.py _inherited_decisions
func inheritedDecisions(t transcript.Transcript, entries []*json.Object, host, session, repoID string,
	keys []string, payload *json.Object, provider policy.ReviewProvider) ([]*json.Object, error) {
	identity := t.Identity
	markerReceipt := ""
	if host == "codex" && t.Brief == nil && identity != nil && identity.AgentID != nil {
		name := (*identity.AgentID)[strings.LastIndexByte(*identity.AgentID, '/')+1:]
		if match := receiptTaskPattern.FindStringSubmatch(name); match != nil {
			markerReceipt = match[1]
		}
	}
	if t.Brief == nil && markerReceipt == "" {
		return nil, nil
	}
	var agentID, expectedParent any
	var parent transcript.Transcript
	if host == "claude" {
		agentID = payload.Value("agent_id")
		if !json.Truthy(agentID) {
			return nil, nil
		}
		path, err := index(payload, "transcript_path")
		if err != nil {
			return nil, err
		}
		if parent, err = load(path, host); err != nil {
			if isOSError(err) {
				return nil, nil
			}
			return nil, err
		}
		// Only the main transcript proves this Claude dispatch, so expectedParent stays None.
	} else {
		if identity == nil || identity.ParentThreadID == nil {
			return nil, nil
		}
		var registered *json.Object
		for _, entry := range slices.Backward(entries) {
			if entry.Value("gate") == "host_identity" && entry.Value("host") == host &&
				entry.Value("thread_id") == any(*identity.ParentThreadID) && entry.Value("repo_id") == repoID {
				registered = entry
				break
			}
		}
		if registered == nil {
			return nil, nil
		}
		path, err := index(registered, "transcript_path")
		if err != nil {
			return nil, err
		}
		if parent, err = load(path, host); err != nil {
			if isOSError(err) {
				return nil, nil
			}
			return nil, err
		}
		if parent.Identity == nil || parent.Identity.ThreadID != *identity.ParentThreadID ||
			parent.Identity.SessionID != session {
			return nil, nil
		}
		if identity.AgentID == nil {
			return nil, &pyerr.Error{Class: "AttributeError", Msg: "'NoneType' object has no attribute 'rsplit'"}
		}
		agentID = *identity.AgentID
		parentPath := *identity.AgentID
		if i := strings.LastIndexByte(parentPath, '/'); i >= 0 { // ASCII separator
			parentPath = parentPath[:i]
		}
		if parentPath != "/root" {
			expectedParent = parentPath
		}
		var parentAgent any
		if parent.Identity.AgentID != nil {
			parentAgent = *parent.Identity.AgentID
		}
		if parentAgent != expectedParent {
			return nil, nil
		}
	}
	assigned := map[string]assignment{}
	for _, call := range parent.ToolCalls {
		if call.Name != "spawn_agent" && call.Name != "Agent" && call.Name != "Task" || call.Output == "" {
			continue
		}
		decoded, err := json.Loads(call.Input)
		if err != nil {
			if errors.As(err, new(*json.RecursionError)) {
				return nil, err
			}
			continue
		}
		args, ok := decoded.(*json.Object)
		if !ok {
			continue
		}
		var message any
		verified := false
		if host == "claude" {
			footer := footerPattern.FindStringSubmatch(call.Output)
			message = args.Value("prompt")
			verified = strings.HasPrefix(call.Output, "[Subagent hand-back]") && footer != nil &&
				agentID == any(footer[1]) && message == any(*t.Brief)
		} else {
			result, err := json.Loads(call.Output)
			if err != nil {
				if errors.As(err, new(*json.RecursionError)) {
					return nil, err
				}
				continue
			}
			if markerReceipt != "" {
				object, ok := result.(*json.Object)
				if call.Name == "spawn_agent" && args.Value("task_name") == "ad_"+markerReceipt &&
					ok && sameValue(object.Value("task_name"), agentID) {
					assigned[markerReceipt] = assignment{}
				}
				continue
			}
			message = args.Value("message")
			text, isStr := message.(string)
			object, isDict := result.(*json.Object)
			verified = isDict && sameValue(object.Value("task_name"), agentID) && isStr &&
				strings.Contains(*t.Brief, text)
		}
		if !verified {
			continue
		}
		text := message.(string) // verified messages are str
		var accepted *string
		if match := acceptedPattern.FindStringSubmatch(text); match != nil {
			accepted = &match[1]
		}
		var refs []string
		if match := referencesPattern.FindStringSubmatch(text); match != nil {
			for value := range strings.SplitSeq(match[1], ",") { // ASCII separator
				refs = append(refs, pystr.Strip(value))
			}
		}
		for _, match := range receiptPattern.FindAllStringSubmatch(text, -1) {
			assigned[match[1]] = assignment{accepted, refs}
		}
	}
	if len(assigned) == 0 {
		return nil, nil
	}
	bundle, err := evidence.Build(parent, keys, evidence.Options{Frame: contextFrame + 2, Profile: provider.Evidence})
	if err != nil {
		return nil, err
	}
	parentContext := gates.Context{Host: host, SessionID: session, AgentID: expectedParent,
		ToolUseID: "parent-observation", Transcript: parent, Evidence: bundle, Entries: entries, RepoID: repoID,
		Metrics: &json.Object{}, Reviewer: provider}
	observedRefs := map[string]bool{}
	for _, call := range parent.ToolCalls {
		if call.Output == "" {
			continue
		}
		isDecide, err := transcript.IsDecide(call)
		if err != nil {
			return nil, err
		}
		if !isDecide {
			observedRefs[call.ID] = true
		}
	}
	consulted, err := gates.Consulted(parentContext)
	if err != nil {
		return nil, err
	}
	var inherited []*json.Object
	for _, entry := range consulted {
		receiptID, _ := entry.Value("receipt_id").(string) // the gates write a str or None
		grant, ok := assigned[receiptID]
		if !ok || entry.Value("repo_id") != repoID {
			continue
		}
		state, err := get(entry, "state", &json.Object{})
		if err != nil {
			return nil, err
		}
		scope, err := get(state, "scope", &json.Object{})
		if err != nil {
			return nil, err
		}
		paths, err := get(scope, "paths", nil)
		if err != nil {
			return nil, err
		}
		if !json.Truthy(paths) {
			continue
		}
		pick, err := index(entry, "pick")
		if err != nil {
			return nil, err
		}
		selected := pick
		if grant.accepted != nil {
			selected = *grant.accepted
		}
		same, err := json.Equal(selected, pick)
		if err != nil {
			return nil, err
		}
		if !same {
			rejected, err := overridden(entry, selected, grant.refs, observedRefs)
			if err != nil {
				return nil, err
			}
			if rejected {
				continue
			}
		}
		options, err := get(state, "options", &json.Object{})
		if err != nil {
			return nil, err
		}
		if listed, err := contains(options, selected); err != nil || !listed {
			if err != nil {
				return nil, err
			}
			continue
		}
		action, err := index(entry, "action")
		if err != nil {
			return nil, err
		}
		refs := make([]any, len(grant.refs))
		for i, ref := range grant.refs {
			refs[i] = ref
		}
		row := entry.Copy()
		for _, field := range []struct {
			key   string
			value any
		}{{"origin", "parent_dispatch"}, {"source_action", action}, {"action", "binding"}, {"pick", selected},
			{"parent_agent_id", expectedParent}, {"inherited_by", agentID}, {"accepted_evidence_refs", refs}} {
			row.Set(field.key, field.value)
		}
		inherited = append(inherited, row)
	}
	return inherited, nil
}

// overridden reports whether a parent's accepted option that differs from the receipt's pick fails to
// qualify: only an eligibility-checked advisory receipt can be overridden, by an eligible option with
// observed evidence. It evaluates the conditions in Python's short-circuit order.
func overridden(entry *json.Object, selected any, refs []string, observed map[string]bool) (bool, error) {
	action, err := index(entry, "action")
	if err != nil || action != "advisory" || !json.Truthy(entry.Value("eligibility_checked")) {
		return true, err
	}
	eligible, err := get(entry, "eligible_options", []any{})
	if err != nil {
		return true, err
	}
	listed, err := contains(eligible, selected)
	if err != nil || !listed {
		return true, err
	}
	return len(refs) == 0 || !allObserved(refs, observed), nil
}

// allObserved reports set(refs).issubset(observed).
func allObserved(refs []string, observed map[string]bool) bool {
	for _, ref := range refs {
		if !observed[ref] {
			return false
		}
	}
	return true
}

// py: agent_decisions/hook.py _save_users
func saveUsers(payload *json.Object, host string, messages, keys []any, frame int) error {
	outcome := gates.Outcome{Action: "captured", Record: json.NewObject(
		"gate", "user_context", "messages", messages, "seen_user_keys", keys)}
	return appendRecord(payload, host, nil, outcome, nil, frame+1)
}

// savedUsers returns the user's words: the signed snapshot of the ledger plus the new host messages that a
// prompt submission or a completed host question confirms. frame is the Python frame of _saved_users.
//
// py: agent_decisions/hook.py _saved_users
func savedUsers(payload *json.Object, host string, t transcript.Transcript, entries []*json.Object,
	frame int) ([]string, error) {
	rawUsers := make([]any, len(t.UserMessages))
	for i, message := range t.UserMessages {
		keys, err := secrets(host)
		if err != nil {
			return nil, err
		}
		rawUsers[i] = evidence.Redact(message, keys)
	}
	rawKeys := make([]any, len(t.UserKeys))
	for i, key := range t.UserKeys {
		rawKeys[i] = key
	}
	if len(rawKeys) == 0 {
		rawKeys = make([]any, len(rawUsers))
		for i, message := range rawUsers {
			rawKeys[i] = strconv.Itoa(i) + ":" + message.(string)
		}
	}
	scope := payload.Value("agent_id")
	var saved *json.Object
	at := -1
	for i, entry := range slices.Backward(entries) {
		if entry.Value("host") != host || entry.Value("gate") != "user_context" {
			continue
		}
		owner := entry.Value("agent_id")
		eligible := owner == nil
		if !eligible && scope != nil {
			var err error
			if eligible, err = json.Equal(owner, scope); err != nil {
				return nil, err
			}
		}
		if eligible {
			saved, at = entry, i
			break
		}
	}
	if saved == nil {
		if err := saveUsers(payload, host, rawUsers, rawKeys, frame+1); err != nil {
			return nil, err
		}
		return strs(rawUsers) // initial history comes from the host's protected transcript
	}
	recorded, err := index(saved, "messages")
	if err != nil {
		return nil, err
	}
	messages, err := iterate(recorded)
	if err != nil {
		return nil, err
	}
	// Older snapshots can contain automatic feedback captured before the parser fix.
	var users []any
	for _, message := range messages {
		text, ok := message.(string)
		if host == "codex" && !ok {
			return nil, &pyerr.Error{Class: "AttributeError",
				Msg: fmt.Sprintf("'%s' object has no attribute 'lstrip'", json.TypeName(message))}
		}
		if host != "codex" || !transcript.IsCodexHookFeedback(text) {
			users = append(users, message)
		}
	}
	seenValue, err := get(saved, "seen_user_keys", []any{})
	if err != nil {
		return nil, err
	}
	seen, err := iterate(seenValue)
	if err != nil {
		return nil, err
	}
	seen = slices.Clone(seen)
	var fresh []int // the indexes of new_users
	for i := range min(len(rawKeys), len(rawUsers)) {
		if !slices.Contains(seen, rawKeys[i]) {
			fresh = append(fresh, i)
		}
	}
	var prompts []any
	for _, entry := range entries[at+1:] {
		if entry.Value("host") == host && entry.Value("gate") == "user_prompt" {
			prompts = append(prompts, entry.Value("prompt"))
		}
	}
	replyKeys := map[string]bool{}
	for _, key := range rawKeys {
		if slices.Contains(seen, key) {
			for _, reply := range t.QuestionReplyKeys {
				replyKeys[reply] = true
			}
			break
		}
	}
	var nativeKeys map[string]bool
	completeNativeHistory := false
	if host == "codex" && len(fresh) > 0 && len(t.QuestionReplyKeys) > 0 {
		nativeKeys = map[string]bool{}
		for _, key := range seen {
			if text, ok := key.(string); ok {
				nativeKeys[text] = true
			}
		}
		var priorNative []any
		for i := range min(len(rawKeys), len(rawUsers)) {
			if nativeKeys[rawKeys[i].(string)] {
				priorNative = append(priorNative, rawUsers[i])
			}
		}
		// Preserve the signed words and their order before adding a historical reply.
		completeNativeHistory, err = json.Equal(users, priorNative)
		if err != nil {
			return nil, err
		}
	}
	reorderReply := false
	for _, i := range fresh {
		key, message := rawKeys[i], rawUsers[i]
		questionReply := host == "codex" && slices.Contains(t.QuestionReplyKeys, key.(string))
		if questionReply && !completeNativeHistory {
			continue
		}
		if at := slices.Index(prompts, message); at >= 0 {
			prompts = slices.Delete(prompts, at, at+1)
		} else if !replyKeys[key.(string)] {
			continue
		}
		if questionReply {
			reorderReply = true
		}
		users = append(users, message)
		seen = append(seen, key)
		if nativeKeys != nil {
			nativeKeys[key.(string)] = true
		}
	}
	if reorderReply {
		users = nil
		for i := range min(len(rawKeys), len(rawUsers)) {
			if nativeKeys[rawKeys[i].(string)] {
				users = append(users, rawUsers[i])
			}
		}
	}
	same, err := json.Equal(users, messages)
	if err != nil {
		return nil, err
	}
	if !same {
		if err := saveUsers(payload, host, users, seen, frame+1); err != nil {
			return nil, err
		}
	}
	return strs(users)
}

// py: agent_decisions/hook.py _secrets
func secrets(host string) ([]string, error) {
	provider, err := ledger.ReviewProvider(host)
	if err != nil {
		return nil, err
	}
	return providerSecrets(provider.Name)
}

func providerSecrets(name string) ([]string, error) {
	loadKey, otherKey := jev.LoadKey, subq.LoadKey
	if name == "subq" {
		loadKey, otherKey = otherKey, loadKey
	}
	var keys []string
	key, err := loadKey()
	if err != nil {
		if !errors.As(err, new(*jev.JevError)) {
			return nil, err
		}
	} else {
		keys = append(keys, key)
	}
	if key, err := otherKey(); err == nil {
		keys = append(keys, key)
	}
	return keys, nil
}

// asker returns the review call of the gates: it reviews a scrubbed copy of state, then makes state that
// copy, as the review saw it. A nil metrics records nothing; otherwise each call's metrics join
// metrics["api_calls"]. frame is the Python frame of the returned ask under hooks/hook.py.
//
// py: agent_decisions/hook.py _asker
func asker(post jev.PostFunc, metrics *json.Object, frame int) gates.Ask {
	return askerWithKey(jev.LoadKey, func(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error) {
		return jev.Ask(state, questions, key, post, metrics)
	}, metrics, frame)
}

func askerWithKey(loadKey func() (string, error), remote func(*json.Object, *json.Object, string, *json.Object) (*json.Object, error),
	metrics *json.Object, frame int) gates.Ask {
	return func(state, questions *json.Object) (answers *json.Object, err error) {
		key, err := loadKey()
		if err != nil {
			return nil, err
		}
		keys := []string{key}
		for _, load := range []func() (string, error){jev.LoadKey, subq.LoadKey} {
			if other, err := load(); err == nil {
				keys = append(keys, other)
			}
		}
		var callMetrics *json.Object
		if metrics != nil {
			callMetrics = &json.Object{}
		}
		scrubbed, err := evidence.Scrub(state, keys, frame+1)
		if err != nil {
			return nil, err
		}
		requestState := scrubbed.(*json.Object)
		defer func() {
			askMu.Lock()
			defer askMu.Unlock()
			for _, name := range state.Keys() {
				state.Delete(name)
			}
			for _, name := range requestState.Keys() {
				state.Set(name, requestState.Value(name))
			}
			if metrics != nil {
				if clean, _, cleanErr := redactKnownIdentifiers(callMetrics, keys); cleanErr == nil {
					callMetrics = clean
				} else {
					callMetrics = json.NewObject("redaction_blocked", true)
					if err == nil {
						answers, err = nil, &jev.JevError{Reason: "redaction"}
					}
				}
				var calls []any
				if value, ok := metrics.Get("api_calls"); ok {
					calls = value.([]any) // only this function writes api_calls
				}
				metrics.Set("api_calls", append(calls, callMetrics))
			}
		}()
		scrubbedQuestions, err := evidence.Scrub(questions, keys, frame+1)
		if err != nil {
			return nil, err
		}
		cleanState, stateIdentifiers, err := redactKnownIdentifiers(requestState, keys)
		if err != nil {
			requestState = json.NewObject("redaction_blocked", true)
			return nil, &jev.JevError{Reason: "redaction"}
		}
		requestState = cleanState
		_, questionIdentifiers, err := redactKnownIdentifiers(scrubbedQuestions.(*json.Object), keys)
		if err != nil || stateIdentifiers || questionIdentifiers {
			return nil, &jev.JevError{Reason: "redaction", Detail: "credential in review identifier"}
		}
		answers, err = remote(requestState, scrubbedQuestions.(*json.Object), key, callMetrics)
		var failure *jev.JevError
		if errors.As(err, &failure) && callMetrics != nil {
			callMetrics.Set("error", failure.Reason)
			if failure.Detail != "" {
				callMetrics.Set("error_detail", failure.Detail)
			}
			switch failure.Phase {
			case "request", "response_body":
				callMetrics.Set("error_phase", failure.Phase)
			}
		}
		return answers, err
	}
}

// appendRecord writes one ledger line for outcome. A nil ctx takes the session and agent from payload.
// frame is the Python frame of _append.
//
// py: agent_decisions/hook.py _append
func appendRecord(payload *json.Object, host string, turn any, outcome gates.Outcome, ctx *gates.Context,
	frame int) error {
	entry := json.NewObject("time", now().UTC().Format("2006-01-02T15:04:05Z"), "host", host)
	if ctx != nil {
		entry.Set("session_id", ctx.SessionID)
		entry.Set("agent_id", ctx.AgentID)
	} else {
		entry.Set("session_id", orUnknown(payload.Value("session_id")))
		entry.Set("agent_id", payload.Value("agent_id"))
	}
	toolUseID, err := index(payload, "tool_use_id")
	if err != nil {
		return err
	}
	entry.Set("tool_use_id", toolUseID)
	entry.Set("turn", turn)
	entry.Set("action", outcome.Action)
	askMu.Lock()
	err = func() error {
		defer askMu.Unlock()
		if ctx != nil {
			keys, err := providerSecrets(ctx.Reviewer.Name)
			if err != nil {
				return err
			}
			metrics, err := evidence.Scrub(ctx.Metrics, keys, frame+1)
			if err != nil {
				return err
			}
			entry.Set("metrics", metrics)
		}
		keys, err := secrets(host)
		if err != nil {
			return err
		}
		if outcome.Record == nil {
			return typeError("'NoneType' object is not a mapping")
		}
		record, err := evidence.Scrub(outcome.Record, keys, frame+1)
		if err != nil {
			return err
		}
		for _, key := range record.(*json.Object).Keys() {
			entry.Set(key, record.(*json.Object).Value(key))
		}
		entry, _, err = redactKnownIdentifiers(entry, keys)
		if err != nil {
			return err
		}
		return nil
	}()
	if err != nil {
		return err
	}
	return ledger.Append(entry)
}

// py: agent_decisions/hook.py _deny
func deny(reason string) *json.Object {
	return json.NewObject("hookSpecificOutput", json.NewObject("hookEventName", "PreToolUse",
		"permissionDecision", "deny", "permissionDecisionReason", pystr.SliceTo(reason, 10_000)))
}

// py: agent_decisions/hook.py _note
func note(text string) *json.Object {
	return json.NewObject("systemMessage", text)
}

// Main runs `agent-decisions hook <event> --host claude|codex` with the payload on stdin and returns the
// exit status: 0 after any response, and 2 for an argparse error.
//
// py: agent_decisions/hook.py main
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	parser := &argparse.Parser{Prog: "agent-decisions hook", Stdout: stdout, Stderr: stderr,
		Actions: []*argparse.Action{argparse.HelpAction(),
			{Kind: argparse.Store, Dest: "event",
				Choices: []string{"pre_tool_use", "stop", "subagent_stop", "session_start", "user_prompt_submit"}},
			{Kind: argparse.Store, Options: []string{"--host"}, Dest: "host", Required: true,
				Choices: []string{"claude", "codex"}}}}
	ns, err := parser.ParseArgs(args)
	if err != nil {
		var exit *argparse.Exit
		if errors.As(err, &exit) {
			return exit.Code
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	event, host := ns["event"].(string), ns["host"].(string)
	out, err := safeHandle(event, host, stdin, stderr)
	if err != nil {
		availability, policyErr := ledger.FailOpen(host)
		var providerErr *policy.ReviewProviderError
		invalidProvider := errors.As(err, &providerErr)
		availability = availability && policyErr == nil && !invalidProvider
		message := "agent-decisions: hook error (" + pyClass(err) + ")"
		if invalidProvider {
			message = "agent-decisions: invalid review provider in signed installation policy"
		}
		switch {
		case availability:
			out = note(message + "; action allowed under availability policy")
		case event == "pre_tool_use":
			out = deny(message + "; required review unavailable, action not run")
		case event == "stop" || event == "subagent_stop":
			out = json.NewObject("decision", "block", "reason", message+"; required final review unavailable")
		default:
			out = note(message + "; required review remains active")
		}
	}
	if os.Getenv("AGENT_DECISIONS_OFF") == "1" {
		if out == nil {
			out = &json.Object{}
		} else {
			out = out.Copy()
		}
		previous, _ := out.Value("systemMessage").(string) // the hook writes only str messages
		out.Set("systemMessage", pystr.Strip(previous+" agent-decisions: AGENT_DECISIONS_OFF ignored; "+
			"review policy is controlled by the protected installer"))
	}
	if out != nil && out.Len() > 0 {
		text, err := json.Dumps(out, json.Options{})
		if err != nil {
			fmt.Fprintln(stderr, err) // a response the hook built always encodes
			return 1
		}
		io.WriteString(stdout, text)
	}
	return 0
}

// safeHandle reads stdin and runs Handle. A panic becomes a RuntimeError after its value and stack go to
// stderr, so it takes the same fail-closed path as a Python exception.
func safeHandle(event, host string, stdin io.Reader, stderr io.Writer) (out *json.Object, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(stderr, "panic: %v\n%s", recovered, debug.Stack())
			out, err = nil, &pyerr.Error{Class: "RuntimeError", Msg: fmt.Sprint(recovered)}
		}
	}()
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, pyerr.NewOSError(err)
	}
	payload, err := json.LoadsBytes(data, stdinFrame)
	if err != nil {
		return nil, err
	}
	return Handle(event, payload, host, jev.Post)
}

// --- Python value semantics of payload and ledger values

// pyClass returns the Python class name of err: its PyType, or RuntimeError for an error without one.
func pyClass(err error) string {
	var typed interface{ PyType() string }
	if errors.As(err, &typed) {
		return typed.PyType()
	}
	if err == nil {
		return ""
	}
	return "RuntimeError"
}

// isOSError reports whether Python's `except OSError` catches err.
func isOSError(err error) bool {
	return errors.As(err, new(*pyerr.OSError))
}

func typeError(format string, args ...any) error {
	return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf(format, args...)}
}

// sameValue returns a == b, treating a comparison that Python would fail on as unequal.
func sameValue(a, b any) bool {
	same, err := json.Equal(a, b)
	return err == nil && same
}

// orUnknown returns `value or "unknown"`.
func orUnknown(value any) any {
	if json.Truthy(value) {
		return value
	}
	return "unknown"
}

// sessionEntries returns ledger.entries(session, cache_limit=policy.CACHED_REVIEWS). ledger._safe raises
// TypeError for a session id that is not a str.
//
// ponytail: Python returns [] before it reaches _safe when no ledger key exists yet; every caller then
// appends to the same session, which raises the same TypeError after creating the key. Go raises here,
// without creating the key.
func sessionEntries(session any) ([]*json.Object, error) {
	text, ok := session.(string)
	if !ok {
		return nil, typeError("expected string or bytes-like object, got '%s'", json.TypeName(session))
	}
	limit := policy.CachedReviews
	return ledger.Entries(text, &limit)
}

// pathArgument returns a payload value used as a path, which must be a str.
//
// ponytail: Python's open() also accepts an int, as a file descriptor; a hook payload never carries one.
func pathArgument(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", typeError("expected str, bytes or os.PathLike object, not %s", json.TypeName(value))
	}
	return text, nil
}

// pyStr returns str(v) for a decoded value used in a file name.
//
// ponytail: str() of a float, list or dict is not ported; agent ids are strings.
func pyStr(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case json.Int:
		return string(v), nil
	case bool:
		if v {
			return "True", nil
		}
		return "False", nil
	}
	return "", typeError("agent_id of type %s is not supported", json.TypeName(v))
}

// get returns v.get(key, fallback), or the AttributeError that Python raises when v is not a dict.
func get(v any, key string, fallback any) (any, error) {
	object, ok := v.(*json.Object)
	if !ok {
		return nil, &pyerr.Error{Class: "AttributeError",
			Msg: fmt.Sprintf("'%s' object has no attribute 'get'", json.TypeName(v))}
	}
	if value, ok := object.Get(key); ok {
		return value, nil
	}
	return fallback, nil
}

// index returns v[key] for a dict v, or Python's KeyError.
func index(v any, key string) (any, error) {
	object, ok := v.(*json.Object)
	if !ok {
		return nil, typeError("'%s' object is not subscriptable", json.TypeName(v))
	}
	value, ok := object.Get(key)
	if !ok {
		return nil, &pyerr.Error{Class: "KeyError", Msg: pystr.Repr(key)}
	}
	return value, nil
}

// iterate returns the items of iter(v): a dict's keys, a list's items or a str's characters.
func iterate(v any) ([]any, error) {
	switch v := v.(type) {
	case []any:
		return v, nil
	case *json.Object:
		keys := make([]any, v.Len())
		for i, key := range v.Keys() {
			keys[i] = key
		}
		return keys, nil
	case string:
		var chars []any
		for i := range pystr.Len(v) {
			chars = append(chars, pystr.Slice(v, i, i+1))
		}
		return chars, nil
	}
	return nil, typeError("'%s' object is not iterable", json.TypeName(v))
}

// contains returns `item in container` for a list, dict or str container.
func contains(container, item any) (bool, error) {
	switch container := container.(type) {
	case []any:
		for _, value := range container {
			if same, err := json.Equal(value, item); same || err != nil {
				return same, err
			}
		}
		return false, nil
	case *json.Object:
		key, ok := item.(string)
		if !ok {
			return false, nil // a dict of a JSON value has str keys only
		}
		_, found := container.Get(key)
		return found, nil
	case string:
		text, ok := item.(string)
		if !ok {
			return false, typeError("'in <string>' requires string as left operand, not %s", json.TypeName(item))
		}
		return strings.Contains(container, text), nil
	}
	return false, typeError("argument of type '%s' is not iterable", json.TypeName(container))
}

// appendStrings appends values, which must be str, to list.
func appendStrings(list []string, values []any) ([]string, error) {
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, typeError("path of type %s is not supported", json.TypeName(value))
		}
		list = append(list, text)
	}
	return list, nil
}

// strs returns values, which must be str, as strings.
func strs(values []any) ([]string, error) {
	return appendStrings(make([]string, 0, len(values)), values)
}

// reversed returns the items of list in reverse order.
func reversed(list []*json.Object) []*json.Object {
	result := slices.Clone(list)
	slices.Reverse(result)
	return result
}
