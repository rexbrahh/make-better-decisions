package hook

import (
	"bytes"
	stdcontext "context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/gates"
	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/subq"
)

type reviewClientFunc func(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error)

func (f reviewClientFunc) Ask(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error) {
	return f(state, questions, key, metrics)
}

func TestSnapAskerSelectsItsClientAndRedactsBothProviderKeys(t *testing.T) {
	setUp(t)
	snapKey := "snap-test-credential-0123456789"
	t.Setenv("SUBQ_API_KEY", snapKey)
	legacyKey := os.Getenv("TYPESAFE_API_KEY")
	saved := newSnapClient
	t.Cleanup(func() { newSnapClient = saved })
	called := 0
	newSnapClient = func(ctx stdcontext.Context) reviewClient {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("review context has no bounded deadline")
		}
		return reviewClientFunc(func(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error) {
			called++
			if key != snapKey {
				t.Fatal("Snap used another provider's credential")
			}
			for _, value := range []*json.Object{state, questions} {
				text := dumps(t, value)
				if strings.Contains(text, snapKey) || strings.Contains(text, legacyKey) {
					t.Fatal("provider credential reached review input")
				}
			}
			return json.NewObject("ok", json.NewObject("type", "noul", "noul", 1.0)), nil
		})
	}
	ctx := makeCtx(t, ctxOptions{})
	ctx.Reviewer, _ = policy.Provider("subq")
	ask, cancel := reviewAsker(ctx, func([]byte, string, time.Duration) ([]byte, error) {
		t.Fatal("Snap fell back to the Jev transport")
		return nil, nil
	}, decideAsk)
	defer cancel()
	state := json.NewObject("observed", snapKey+" "+legacyKey)
	questions := json.NewObject("ok", json.NewObject("type", "noul", "instructions", snapKey+" "+legacyKey))
	if _, err := ask(state, questions); err != nil || called != 1 {
		t.Fatalf("calls %d, error %v", called, err)
	}
	if text := dumps(t, state); strings.Contains(text, snapKey) || strings.Contains(text, legacyKey) {
		t.Fatal("unscrubbed state reached the caller")
	}
}

func TestSessionStartNamesTheSignedReviewProvider(t *testing.T) {
	setUp(t)
	if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", "codex", "review_provider", "subq"))); err != nil {
		t.Fatal(err)
	}
	out := handle(t, "session_start", json.NewObject("session_id", "s"), "codex", fake(nil).Post)
	assertContains(t, dumps(t, out), "SubQ Snap reviews declared edit tools")
}

func TestToolInputCannotSelectTheReviewProvider(t *testing.T) {
	dir := setUp(t)
	path := writeJSONL(t, dir, "native.jsonl", []*json.Object{human("Inspect the parser facts.")}, "")
	ctx, err := context(json.NewObject("session_id", "s", "transcript_path", path, "tool_use_id", "read",
		"review_provider", "subq", "tool_input", json.NewObject("review_provider", "subq"), "cwd", dir), "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Reviewer.Name != "jev" {
		t.Fatal("tool input changed the provider")
	}
}

func TestSnapContextDoesNotDependOnAnInvalidJevCredential(t *testing.T) {
	dir := setUp(t)
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("SUBQ_API_KEY", "snap-test-credential-0123456789")
	path, err := policy.KeyFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", "claude", "review_provider", "subq"))); err != nil {
		t.Fatal(err)
	}
	transcript := writeJSONL(t, dir, "native.jsonl", []*json.Object{human("Inspect the parser facts.")}, "")
	ctx, err := context(json.NewObject("session_id", "s", "transcript_path", transcript,
		"tool_use_id", "read", "cwd", dir), "claude", nil)
	if err != nil || ctx.Reviewer.Name != "subq" {
		t.Fatalf("Snap context depended on an unused credential: %v", err)
	}
}

func TestSnapContextRetainsItsLargerEvidenceAfterFinalTrimming(t *testing.T) {
	dir := setUp(t)
	if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", "claude", "review_provider", "subq"))); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "large.py"), strings.Repeat("# retained source observation\n", 4_000))
	path := writeJSONL(t, dir, "native.jsonl", []*json.Object{human("Inspect large.py and preserve its source observations.")}, "")
	ctx, err := context(json.NewObject("session_id", "s", "transcript_path", path, "tool_use_id", "read",
		"cwd", dir, "tool_input", json.NewObject("file_path", "large.py")), "claude", []string{"large.py"})
	if err != nil {
		t.Fatal(err)
	}
	text := dumps(t, ctx.Evidence)
	if len(text) <= policy.EvidenceChars || len(text) > ctx.Reviewer.Evidence.BudgetChars {
		t.Fatalf("Snap evidence size %d does not use its selected budget", len(text))
	}
	assertEqual(t, at(t, ctx.Evidence, "metadata", "evidence_budget_chars"), json.Int("800000"))
}

func TestNativeInheritanceRejectsAReceiptFromThePreviousProvider(t *testing.T) {
	dir := setUp(t)
	prepared := rcPrepare(t, dir, rcPrepareOptions{})
	if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", "codex", "review_provider", "subq"))); err != nil {
		t.Fatal(err)
	}
	ctx := rcContext(t, prepared.payload(), "codex")
	if len(ctx.InheritedEntries) != 0 {
		t.Fatal("a Jev receipt crossed the owner provider switch")
	}
}

func TestNativeInheritanceAcceptsOnlyTheCurrentSnapReceipt(t *testing.T) {
	dir := setUp(t)
	t.Setenv("SUBQ_API_KEY", "snap-test-credential-0123456789")
	saved := newSnapClient
	t.Cleanup(func() { newSnapClient = saved })
	newSnapClient = func(stdcontext.Context) reviewClient {
		return reviewClientFunc(func(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error) {
			return jev.Ask(state, questions, key, fake(map[string]any{"pick": "chunks", "sufficient": .95,
				"owner": "agent", "invariant_0_chunks": .95, "invariant_0_whole": .95}).Post, metrics)
		})
	}
	configure := func(provider string) {
		t.Helper()
		if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
			"spec", json.NewObject("host", "codex", "review_provider", provider))); err != nil {
			t.Fatal(err)
		}
	}
	configure("subq")
	prepared := rcPrepare(t, dir, rcPrepareOptions{})
	if got := len(rcContext(t, prepared.payload(), "codex").InheritedEntries); got != 1 {
		t.Fatalf("matching Snap parent receipt count = %d", got)
	}
	configure("jev")
	if got := len(rcContext(t, prepared.payload(), "codex").InheritedEntries); got != 0 {
		t.Fatal("a Snap receipt crossed the owner provider switch")
	}
}

func TestReviewCannotExportCredentialIdentifiers(t *testing.T) {
	setUp(t)
	snapKey := "snap-test-credential-0123456789"
	t.Setenv("SUBQ_API_KEY", snapKey)
	legacyKey := os.Getenv("TYPESAFE_API_KEY")
	for _, key := range []string{snapKey, legacyKey} {
		calls := 0
		ask := askerWithKey(subq.LoadKey, func(*json.Object, *json.Object, string, *json.Object) (*json.Object, error) {
			calls++
			return json.NewObject(), nil
		}, nil, decideAsk)
		state := json.NewObject("observations", []any{json.NewObject(key, "data")})
		_, err := ask(state, json.NewObject("ok", json.NewObject("type", "noul")))
		var failure *jev.JevError
		if calls != 0 || !errors.As(err, &failure) || failure.Reason != "redaction" {
			t.Fatalf("credential identifier reached review: calls=%d, error=%v", calls, err)
		}
		if strings.Contains(dumps(t, state), key) {
			t.Fatal("failed review retained a credential identifier")
		}
	}
}

func TestLedgerRedactsCredentialKeysWithoutCollisions(t *testing.T) {
	setUp(t)
	snapKey := "snap-test-credential-0123456789"
	t.Setenv("SUBQ_API_KEY", snapKey)
	legacyKey := os.Getenv("TYPESAFE_API_KEY")
	fields := json.NewObject(snapKey, "snap", legacyKey, "jev", "<redacted identifier 1>", "original")
	err := appendRecord(json.NewObject("session_id", "s", "tool_use_id", "check"), "claude", nil,
		gates.Outcome{Action: "blocked", Record: json.NewObject("gate", "test", "attributes", fields)}, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	rows := entries(t, "s")
	text := dumps(t, rows[0])
	if strings.Contains(text, snapKey) || strings.Contains(text, legacyKey) {
		t.Fatal("credential identifier reached the ledger")
	}
	attributes := rows[0].Value("attributes").(*json.Object)
	if attributes.Len() != 3 || attributes.Value("<redacted identifier 1>") != "original" {
		t.Fatal("redaction changed or merged unrelated identifiers")
	}
}

func TestRemoteMetricsCannotPersistCredentialsAfterRotation(t *testing.T) {
	setUp(t)
	key := "snap-test-credential-0123456789"
	t.Setenv("SUBQ_API_KEY", key)
	ctx := makeCtx(t, ctxOptions{})
	ctx.Reviewer, _ = policy.Provider("subq")
	ctx.Metrics = json.NewObject()
	ask := askerWithKey(subq.LoadKey, func(_ *json.Object, _ *json.Object, _ string, metrics *json.Object) (*json.Object, error) {
		metrics.Set("usage", json.NewObject(key, json.Int("1")))
		metrics.Set("returned_metadata", key)
		t.Setenv("SUBQ_API_KEY", "rotated-snap-credential-0123456789")
		return json.NewObject(), nil
	}, ctx.Metrics, decideAsk)
	if _, err := ask(json.NewObject(), json.NewObject("ok", json.NewObject("type", "noul"))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dumps(t, ctx.Metrics), key) {
		t.Fatal("remote metric retained a credential after key rotation")
	}
	if err := appendRecord(json.NewObject("session_id", ctx.SessionID, "tool_use_id", "check"), "claude", nil,
		gates.Outcome{Action: "blocked", Record: json.NewObject("gate", "test")}, &ctx, 4); err != nil {
		t.Fatal(err)
	}
	rows := entries(t, ctx.SessionID)
	if len(rows) != 1 || strings.Contains(dumps(t, rows[0]), key) {
		t.Fatal("remote metric credential reached the signed ledger")
	}
}

func TestInvalidProviderCannotUseAvailabilityModeForReviewedTools(t *testing.T) {
	for _, host := range []string{"codex", "claude"} {
		t.Run(host, func(t *testing.T) {
			dir := setUp(t)
			if err := ledger.Append(json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
				"spec", json.NewObject("host", host, "review_mode", "availability", "review_provider", "unknown"))); err != nil {
				t.Fatal(err)
			}
			rows := []*json.Object{human("Inspect the parser facts.")}
			if host == "codex" {
				rows = []*json.Object{rcNativeMetadata("s", "s", dir, "", ""), codexUser("Inspect the parser facts.")}
			}
			path := writeJSONL(t, dir, "native.jsonl", rows, "")
			for _, tool := range []string{"Edit", "Bash", "ExitPlanMode", "Read"} {
				if tool == "ExitPlanMode" && host != "claude" {
					continue
				}
				input := json.NewObject("file_path", "parser.py", "new_string", "read_chunks()")
				if tool == "Bash" {
					input = json.NewObject("command", `ts-decide '{"decision":"Which cache?","options":[{"id":"a","description":"A"},{"id":"b","description":"B"}]}'`)
				}
				payload := json.NewObject("session_id", "s", "tool_use_id", "check", "tool_name", tool,
					"tool_input", input, "transcript_path", path, "cwd", dir)
				for _, transcriptPath := range []string{path, filepath.Join(dir, "missing.jsonl")} {
					payload.Set("transcript_path", transcriptPath)
					var stdout, stderr bytes.Buffer
					code := Main([]string{"pre_tool_use", "--host", host}, strings.NewReader(dumps(t, payload)), &stdout, &stderr)
					if code != 0 {
						t.Fatalf("%s exit %d: %s", tool, code, stderr.String())
					}
					if tool == "Read" {
						if stdout.Len() != 0 {
							t.Fatalf("ordinary read was intercepted: %s", stdout.String())
						}
						continue
					}
					if permission(obj(t, stdout.String())) != "deny" {
						t.Fatalf("%s used availability for invalid provider policy with %s: %s", tool, filepath.Base(transcriptPath), stdout.String())
					}
				}
			}
		})
	}
}

func TestFailedSnapFragmentCancelsItsPeersAndRetainsTheCause(t *testing.T) {
	setUp(t)
	t.Setenv("SUBQ_API_KEY", "snap-test-credential-0123456789")
	saved := newSnapClient
	t.Cleanup(func() { newSnapClient = saved })
	started := make(chan struct{})
	newSnapClient = func(ctx stdcontext.Context) reviewClient {
		return reviewClientFunc(func(state, questions *json.Object, key string, metrics *json.Object) (*json.Object, error) {
			if state.Value("fragment") == "slow" {
				close(started)
				<-ctx.Done()
				return nil, &jev.JevError{Reason: "cancelled"}
			}
			<-started
			return nil, &jev.JevError{Reason: "http_402"}
		})
	}
	ctx := makeCtx(t, ctxOptions{})
	ctx.Reviewer, _ = policy.Provider("subq")
	ask, cancel := reviewAsker(ctx, fake(nil).Post, editAsk)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	results := make(chan error, 2)
	for _, fragment := range []string{"slow", "failed"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := ask(json.NewObject("fragment", fragment), json.NewObject("ok", json.NewObject("type", "noul")))
			results <- err
		}()
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case err := <-results:
			var failure *jev.JevError
			if !errors.As(err, &failure) || failure.Reason != "http_402" {
				t.Fatalf("fragment lost the original cause: %v", err)
			}
		case <-deadline.C:
			t.Fatal("failed fragment left its peer running")
		}
	}
}
