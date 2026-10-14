// Command agent-decisions is the single binary of the hook, the installer and the isolation commands.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/rexbrahh/make-better-decisions/internal/hook"
	"github.com/rexbrahh/make-better-decisions/internal/install"
	"github.com/rexbrahh/make-better-decisions/internal/isolation"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = "usage: agent-decisions {hook,install,isolate,shell-prefix,version} ...\n"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches one subcommand and returns its exit status.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		io.WriteString(stderr, usage)
		return 2
	}
	rest := args[1:]
	switch args[0] {
	case "hook":
		return hook.Main(rest, stdin, stdout, stderr)
	case "install":
		return install.Main(rest, stdout, stderr)
	case "isolate":
		return isolation.MainIsolate(rest, stdin, stdout, stderr)
	case "shell-prefix":
		return isolation.MainShellPrefix(rest, stdin, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	}
	fmt.Fprintf(stderr, "%sagent-decisions: unknown command %q\n", usage, args[0])
	return 2
}
