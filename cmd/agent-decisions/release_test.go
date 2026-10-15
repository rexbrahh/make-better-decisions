package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseBuildChecksBothManifestVersions(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, diagnostic string
	}{
		{"matching", "{\n  \"version\": \"2.0.1\"\n}\n", ""},
		{"mismatch", "{\n  \"version\": \"2.0.2\"\n}\n", "manifest versions differ"},
		{"missing_version", "{}\n", "cannot read the version from .codex-plugin/plugin.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, previous := releaseBuildFixture(t, `#!/bin/sh
set -eu
echo called >> go-calls
if [ "$1" = env ]; then echo go1.27.2; exit 0; fi
while [ "$1" != -o ]; do shift; done
echo new > "$2"
`)
			if err := os.WriteFile(filepath.Join(dir, ".codex-plugin/plugin.json"), []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("/bin/sh", "scripts/build-release")
			command.Dir = dir
			command.Env = []string{"PATH=" + filepath.Join(dir, "fake-bin") + ":/usr/bin:/bin", "TMPDIR=" + dir}
			output, err := command.CombinedOutput()
			if tc.diagnostic == "" {
				if err != nil {
					t.Fatalf("matching manifests: %v, %s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), tc.diagnostic) {
				t.Fatalf("invalid manifests: %v, %s; want %q", err, output, tc.diagnostic)
			}
			if _, err := os.Stat(filepath.Join(dir, "go-calls")); !os.IsNotExist(err) {
				t.Fatalf("invalid manifests invoked Go: %v", err)
			}
			for _, name := range []string{"dist/agent-decisions-darwin-arm64", "dist/agent-decisions-linux-amd64", "bin/agent-decisions.sha256"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(data) != previous[name] {
					t.Errorf("invalid manifests changed %s: %v", name, err)
				}
			}
		})
	}
}

func releaseBuildFixture(t *testing.T, goScript string) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repo, "scripts", "build-release"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"scripts/build-release":             string(script),
		".claude-plugin/plugin.json":        "{\n  \"version\": \"2.0.1\"\n}\n",
		".codex-plugin/plugin.json":         "{\n  \"version\": \"2.0.1\"\n}\n",
		"bin/agent-decisions.sha256":        "previous pins\n",
		"dist/agent-decisions-darwin-arm64": "previous darwin\n",
		"dist/agent-decisions-linux-amd64":  "previous linux\n",
		"fake-bin/go":                       goScript,
		"fake-bin/shasum":                   "#!/bin/sh\necho \"fixture-hash  $3\"\n",
	}
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, files
}

func TestReleaseBuildFailurePreservesPreviousAssets(t *testing.T) {
	dir, files := releaseBuildFixture(t, `#!/bin/sh
set -eu
if [ "$1" = env ]; then
  echo go1.27.2
  exit 0
fi
[ "$GOOS" = darwin ] || exit 42
while [ "$1" != -o ]; do shift; done
echo 'new darwin' > "$2"
`)
	command := exec.Command("/bin/sh", "scripts/build-release")
	command.Dir = dir
	command.Env = []string{"PATH=" + filepath.Join(dir, "fake-bin") + ":/usr/bin:/bin", "TMPDIR=" + dir}
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 42 {
		t.Fatalf("second-target failure: %v, %s", err, output)
	}
	for _, name := range []string{"dist/agent-decisions-darwin-arm64", "dist/agent-decisions-linux-amd64", "bin/agent-decisions.sha256"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != files[name] {
			t.Errorf("failed build changed %s: %v", name, err)
		}
	}
	staging, err := filepath.Glob(filepath.Join(dir, ".release-build.*"))
	if err != nil || len(staging) != 0 {
		t.Errorf("failed build left staging directories: %v, %v", staging, err)
	}
}
