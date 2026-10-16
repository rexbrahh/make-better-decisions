package gates

import (
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/jev"
	"github.com/rexbrahh/make-better-decisions/internal/jev/jevtest"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/transcript"
)

func TestReviewProviderChangesInvalidateGateAnswers(t *testing.T) {
	setUp(t)
	first := questionGate(t, obj(t, colour), makeCtx(t, ctxOptions{}), asker(fake(map[string]any{"owner_0": "user"})))
	entry := entryOf(first.Record, "host", "claude", "agent_id", nil, "action", first.Action)
	ctx := makeCtx(t, ctxOptions{entries: []*json.Object{entry}})
	ctx.Reviewer, _ = policy.Provider("subq")
	api := fake(map[string]any{"owner_0": "user"})
	questionGate(t, obj(t, colour), ctx, asker(api))
	if len(api.Requests()) != 1 {
		t.Fatal("Snap reused a Jev gate answer")
	}
}

func TestCompletedAndInheritedReceiptsStayWithTheirReviewIdentity(t *testing.T) {
	setUp(t)
	verdict := "ts-decide verdict: ADVISORY\nreceipt: review_fixture"
	entry := json.NewObject("host", "claude", "agent_id", nil, "gate", "decide", "action", "advisory",
		"tool_use_id", "consult", "receipt_id", "review_fixture", "verdict", verdict)
	tr := transcript.Transcript{ToolCalls: []transcript.ToolCall{{ID: "consult", Name: "Bash",
		Input: `{"command":"ts-decide '{}'"}`, Output: verdict}}}
	legacy := makeCtx(t, ctxOptions{transcript: &tr, entries: []*json.Object{entry}})
	snap := legacy
	snap.Reviewer, _ = policy.Provider("subq")
	for _, ctx := range []Context{snap, legacy} {
		ctx.InheritedEntries = []*json.Object{entry}
		rows, err := Consulted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if ctx.Reviewer.Name == "subq" {
			want = 0
		}
		if len(rows) != want {
			t.Fatalf("provider %q received %d receipts, want %d", ctx.Reviewer.Name, len(rows), want)
		}
	}
	entry.Set("review_identity", reviewIdentity(snap))
	for _, row := range snap.Entries {
		row.Set("review_identity", reviewIdentity(snap))
	}
	for _, change := range []string{"none", "provider", "model", "profile"} {
		ctx := snap
		switch change {
		case "provider":
			ctx.Reviewer, _ = policy.Provider("jev")
		case "model":
			ctx.Reviewer.Model = "subq-next"
		case "profile":
			ctx.Reviewer.ContextProfile = "snap-next"
		}
		ctx.InheritedEntries = []*json.Object{entry}
		rows, err := Consulted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if change == "none" {
			want = 2
		}
		if len(rows) != want {
			t.Fatalf("%s: got %d receipts, want %d", change, len(rows), want)
		}
	}
}

func TestSnapFailuresNameTheSelectedProvider(t *testing.T) {
	setUp(t)
	ctx := makeCtx(t, ctxOptions{})
	ctx.Reviewer, _ = policy.Provider("subq")
	api := &jevtest.Fake{Raises: &jev.JevError{Reason: "unreachable"}}
	out := decide(t, progressRequest, ctx, asker(api))
	assertAction(t, out, "unverified")
	assertContains(t, out.Message, "subq unreachable")
	out = editGate(t, "Edit", json.NewObject("file_path", "parser.py", "new_string", "read_chunks()"), ctx, asker(api))
	assertAction(t, out, "deny")
	assertContains(t, out.Message, "subq unreachable")
}
