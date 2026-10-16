package policy

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReviewProviderProfiles(t *testing.T) {
	for _, name := range []string{"", "jev"} {
		got, err := Provider(name)
		if err != nil || got.Name != "jev" || got.Model != Model || got.Timeout != 3*time.Second ||
			got.Evidence.BudgetChars != EvidenceChars || got.Evidence.TextChars != ClipChars {
			t.Fatalf("legacy provider %q: %+v, %v", name, got, err)
		}
	}
	got, err := Provider("subq")
	if err != nil || got.Name != "subq" || got.Model != "subq-snap-preview" || got.Timeout != 10*time.Second ||
		got.ContextProfile != "snap-2" || got.Evidence.BudgetChars != 800_000 || got.Evidence.TextChars != 160_000 {
		t.Fatalf("Snap provider: %+v, %v", got, err)
	}
	for _, name := range []string{"SUBQ", "unknown", "https://example.invalid"} {
		if _, err := Provider(name); err == nil {
			t.Fatalf("unknown provider %q accepted", name)
		}
	}
}

func TestSubQKeyFileUsesTheManagedSecretPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := SubQKeyFile()
	if want := filepath.Join(home, ".config/sops-nix/secrets/subq_api_key"); err != nil || got != want {
		t.Fatalf("key path = %q, %v; want %q", got, err, want)
	}
}
