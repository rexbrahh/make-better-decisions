package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReleaseBuildFailurePreservesPreviousAssets(t *testing.T) {
	dir := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repo, "scripts", "build-release"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"scripts/build-release":             string(script),
		".claude-plugin/plugin.json":        "{\n  \"version\": \"2.0.1\"\n}\n",
		"bin/agent-decisions.sha256":        "previous pins\n",
		"dist/agent-decisions-darwin-arm64": "previous darwin\n",
		"dist/agent-decisions-linux-amd64":  "previous linux\n",
		"fake-bin/go": `#!/bin/sh
set -eu
if [ "$1" = env ]; then
  echo go1.27.2
  exit 0
fi
[ "$GOOS" = darwin ] || exit 42
while [ "$1" != -o ]; do shift; done
echo 'new darwin' > "$2"
`,
		"fake-bin/shasum": "#!/bin/sh\necho \"fixture-hash  $3\"\n",
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
