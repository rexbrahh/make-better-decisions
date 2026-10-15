package oracletest

import "testing"

func TestCLIPresentationPreservesSemanticDifferences(t *testing.T) {
	for _, c := range []struct{ input, want string }{
		{"usage: tool [-h]\n\noptions:\n  -h  show help\n", "usage:tool[-h]options:-hshowhelp"},
		{"usage: tool\n\x1b[35mtool: error:\x1b[0m invalid choice: 'clade', maybe you meant 'claude'? (choose from 'claude', 'codex')\n",
			"tool: error: invalid choice: 'clade' (choose from 'claude', 'codex')\n"},
		{"usage: tool\ntool: error: invalid choice: 'a  b'\n", "tool: error: invalid choice: 'a  b'\n"},
		{`{"reason":"usage: tool: error: keep this exactly"}`, `{"reason":"usage: tool: error: keep this exactly"}`},
	} {
		if got := CLIPresentation(c.input); got != c.want {
			t.Errorf("CLIPresentation(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
