package install

import (
	"bytes"
	"testing"

	"github.com/rexbrahh/make-better-decisions/internal/ledger"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

func TestReviewProviderSelectionPreservesAnOmittedFlag(t *testing.T) {
	dir := setUp(t)
	entry, err := InstallationEntry("codex", dir+"/state", json.NewObject(), []any{}, nil, nil)
	check(t, err)
	check(t, ConfigureReviewProvider(entry, nil, nil))
	if _, exists := entry.Value("spec").(*json.Object).Get("review_provider"); exists {
		t.Fatal("legacy policy changed")
	}
	prior := json.NewObject("review_provider", "subq")
	check(t, ConfigureReviewProvider(entry, prior, nil))
	wantEqual(t, dig(entry, "spec", "review_provider"), "subq")
	selected := "jev"
	check(t, ConfigureReviewProvider(entry, prior, &selected))
	wantEqual(t, dig(entry, "spec", "review_provider"), "jev")
	for _, bad := range []any{"unknown", "", nil} {
		if err := ConfigureReviewProvider(entry, json.NewObject("review_provider", bad), nil); err == nil {
			t.Fatalf("invalid previous selection %v accepted", bad)
		}
	}
}

func TestBothHostPlansPreserveTheReviewProvider(t *testing.T) {
	dir := setUp(t)
	prior := json.NewObject("review_provider", "subq")
	codex, err := BuildCodexPlan(nil, dir+"/state", dir+"/plugin", nil, nil, dir, prior)
	check(t, err)
	claude, err := BuildClaudePlan(json.NewObject(), dir+"/state", dir+"/plugin", dir, nil, nil, prior)
	check(t, err)
	for _, plan := range []*json.Object{codex, claude} {
		wantEqual(t, dig(plan, "installation_entry", "spec", "review_provider"), "subq")
	}
}

func TestReviewProviderFlagOnlyChangesTheSignedPlan(t *testing.T) {
	dir := setUp(t)
	writeFile(t, dir+"/mcp.json", `{"servers": []}`)
	writeFile(t, dir+"/config.toml", "")
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--host", "codex", "--state-root", dir + "/state", "--install-root", dir + "/installed",
		"--review-provider", "subq", "--codex-mcp-json", dir + "/mcp.json", "--codex-config", dir + "/config.toml"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	wantEqual(t, dig(load(t, stdout.String()), "plans", 0, "installation_entry", "spec", "review_provider"), "subq")
}

func TestReviewProviderFlagPreservesLegacyReviewModeAbbreviations(t *testing.T) {
	setUp(t)
	for _, prefix := range []string{"--r", "--re", "--review", "--review-", "--review-m"} {
		var stdout, stderr bytes.Buffer
		parser, err := newParser(&stdout, &stderr)
		check(t, err)
		ns, err := parser.ParseArgs([]string{prefix, "availability"})
		if err != nil || ns["review_mode"] != "availability" || ns["review_provider"] != nil {
			t.Fatalf("legacy %s changed: %v, %s", prefix, err, stderr.String())
		}
	}
}

func TestExplicitReviewProviderRepairsInvalidPriorSelection(t *testing.T) {
	dir := setUp(t)
	for _, host := range []string{"codex", "claude"} {
		entry, err := InstallationEntry(host, dir+"/state", json.NewObject(), []any{}, nil, nil)
		check(t, err)
		entry.Value("spec").(*json.Object).Set("review_provider", "unknown")
		check(t, ledger.Append(entry))
	}
	writeFile(t, dir+"/mcp.json", `{"servers": []}`)
	writeFile(t, dir+"/config.toml", "")
	writeFile(t, dir+"/settings.json", `{}`)
	writeFile(t, dir+"/claude.json", `{}`)
	args := []string{"--host", "both", "--state-root", dir + "/state", "--install-root", dir + "/installed",
		"--codex-mcp-json", dir + "/mcp.json", "--codex-config", dir + "/config.toml",
		"--claude-settings", dir + "/settings.json", "--claude-json", dir + "/claude.json"}
	var stdout, stderr bytes.Buffer
	if code := Main(args, &stdout, &stderr); code == 0 {
		t.Fatal("omitted selection silently repaired invalid policy")
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main(append(args, "--review-provider", "subq"), &stdout, &stderr); code != 0 {
		t.Fatalf("explicit owner repair exit %d: %s", code, stderr.String())
	}
	for i, host := range []string{"codex", "claude"} {
		wantEqual(t, dig(load(t, stdout.String()), "plans", i, "installation_entry", "spec", "review_provider"), "subq")
		previous, err := ledger.InstallationPolicy(host)
		check(t, err)
		wantEqual(t, previous.Value("review_provider"), "unknown")
	}
}
