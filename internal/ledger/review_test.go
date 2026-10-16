package ledger

import (
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

func TestReviewProviderUsesOnlySignedHostPolicy(t *testing.T) {
	useState(t)
	t.Setenv("AGENT_DECISIONS_REVIEW_PROVIDER", "subq")
	got, err := ReviewProvider("codex")
	if err != nil || got.Name != "jev" {
		t.Fatalf("legacy policy: %+v, %v", got, err)
	}
	mustAppend(t, json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
		"spec", json.NewObject("host", "codex", "review_provider", "subq")))
	for host, want := range map[string]string{"codex": "subq", "claude": "jev"} {
		got, err := ReviewProvider(host)
		if err != nil || got.Name != want {
			t.Fatalf("%s: %+v, %v; want %s", host, got, err, want)
		}
	}
}

func TestUnknownSignedReviewProviderFailsClosed(t *testing.T) {
	for _, value := range []any{"unknown", "", nil, json.Int("1")} {
		t.Run(json.TypeName(value), func(t *testing.T) {
			useState(t)
			mustAppend(t, json.NewObject("session_id", "installation", "gate", "installation", "action", "configured",
				"spec", json.NewObject("host", "codex", "review_provider", value)))
			if _, err := ReviewProvider("codex"); err == nil {
				t.Fatalf("invalid provider %v accepted", value)
			}
		})
	}
}
