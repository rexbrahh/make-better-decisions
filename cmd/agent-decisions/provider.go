package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

func runProvider(args []string, stdin io.Reader, stdout, stderr io.Writer,
	installer func([]string, io.Writer, io.Writer) int) int {
	if len(args) != 0 {
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Fprintln(stdout, "usage: agent-decisions provider\nChoose a review model and host, preview the plan, then type apply to activate it.")
			return 0
		}
		fmt.Fprintln(stderr, "usage: agent-decisions provider")
		return 2
	}

	reader := bufio.NewReader(stdin)
	read := func(prompt string) (string, error) {
		if _, err := io.WriteString(stdout, prompt); err != nil {
			return "", err
		}
		line, err := reader.ReadSlice('\n')
		if err == io.EOF {
			return "", nil
		}
		return strings.TrimSpace(string(line)), err
	}
	cancel := func(err error) int {
		if err != nil {
			fmt.Fprintln(stderr, "agent-decisions provider:", err)
			return 1
		}
		if _, err := fmt.Fprintln(stdout, "Cancelled. No changes applied."); err != nil {
			fmt.Fprintln(stderr, "agent-decisions provider:", err)
			return 1
		}
		return 0
	}

	model, err := read("Choose your review model:\n1) SubQ Snap — default, larger evidence, preview\n2) Jev — compact evidence\nChoice (blank cancels): ")
	if err != nil || model == "" {
		return cancel(err)
	}
	provider := map[string]string{"1": "subq", "2": "jev"}[model]
	if provider == "" {
		fmt.Fprintln(stderr, "Select 1 or 2.")
		return 2
	}
	hostChoice, err := read("Choose the host:\n1) Codex\n2) Claude Code\n3) Both\nChoice (blank cancels): ")
	if err != nil || hostChoice == "" {
		return cancel(err)
	}
	host := map[string]string{"1": "codex", "2": "claude", "3": "both"}[hostChoice]
	if host == "" {
		fmt.Fprintln(stderr, "Select 1, 2 or 3.")
		return 2
	}
	installArgs := []string{"--host", host, "--review-provider", provider}
	var preview bytes.Buffer
	if code := installer(installArgs, &preview, stderr); code != 0 {
		return code
	}
	if _, err := io.Copy(stdout, &preview); err != nil {
		return cancel(err)
	}
	confirmation, err := read("Type apply to activate this selection (blank cancels): ")
	if err != nil || confirmation != "apply" {
		return cancel(err)
	}
	return installer(append(installArgs, "--apply"), stdout, stderr)
}
