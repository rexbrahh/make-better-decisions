// Package oracletest locates the Python implementation that the oracle tests compare the Go port with.
//
// The Python sources left this repository when the Go engine replaced them. To run the oracle tests, check out
// the last commit that holds them and name that checkout in AGENT_DECISIONS_PYTHON_SOURCE:
//
//	git worktree add "$TMPDIR/agent-decisions-python" python-final
//	AGENT_DECISIONS_PYTHON=/opt/homebrew/bin/python3 AGENT_DECISIONS_PYTHON_SOURCE="$TMPDIR/agent-decisions-python" go test ./...
package oracletest

import (
	"os"
	"path/filepath"
	"testing"
)

// Source returns the resolved root of the Python checkout, the directory that holds agent_decisions/. It skips the
// test when AGENT_DECISIONS_PYTHON_SOURCE is unset and fails it when the directory holds no Python package.
func Source(t testing.TB) string {
	t.Helper()
	source := os.Getenv("AGENT_DECISIONS_PYTHON_SOURCE")
	if source == "" {
		t.Skip("set AGENT_DECISIONS_PYTHON_SOURCE to a checkout of the Python implementation to compare with the oracle")
	}
	source, err := filepath.EvalSymlinks(source) // the Python module paths that the oracle reports are resolved
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "agent_decisions/__init__.py")); err != nil {
		t.Fatalf("AGENT_DECISIONS_PYTHON_SOURCE holds no Python implementation: %v", err)
	}
	return source
}
