package install

// The subset of CPython 3.15's argparse that the installer parser uses: help, store, store_true and
// append optionals, abbreviated options, the errors with their close-match suggestions, and the usage and
// help text with its wrapping and colors. It adapts the private port in internal/isolation, which has a
// subcommand instead of append options.

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	pyre "github.com/rexbrahh/make-better-decisions/internal/py/re"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

type actionKind int

const (
	helpAction      actionKind = iota // action="help", nargs=0
	storeAction                       // the default action, nargs=None
	storeTrueAction                   // action="store_true", nargs=0
	appendAction                      // action="append", nargs=None
)

// action is an argparse Action with option strings.
type action struct {
	kind     actionKind
	options  []string
	dest     string
	choices  []string // nil is None
	metavar  string   // "" is None
	help     string   // "" is None
	fallback any      // the default: nil, a string, false or an empty list
}

type parser struct {
	prog           string
	actions        []*action
	stdout, stderr io.Writer
}

// namespace is an argparse.Namespace: dest to value, where nil is None.
type namespace map[string]any

// argumentError is argparse.ArgumentError. A nil action has no argument name.
type argumentError struct {
	action *action
	msg    string
}

func (e *argumentError) Error() string {
	if e.action == nil {
		return e.msg
	}
	return "argument " + strings.Join(e.action.options, "/") + ": " + e.msg
}

// exitStatus is the SystemExit that argparse raises after it prints help.
type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(s)) }

// negativeNumber is _negative_number_matcher. The parser has no option that looks like a negative number,
// so an argument that matches it is positional.
var negativeNumber = regexp.MustCompile(`\A-\.?` + pyre.Digit)

// newParser builds the parser of cli. Python evaluates the defaults as it adds the arguments, so their
// errors escape even --help.
//
// py: agent_decisions/install.py cli
func newParser(stdout, stderr io.Writer) (*parser, error) {
	stateDir, err := policy.StateDir()
	if err != nil {
		return nil, err
	}
	installRoot, err := DefaultInstallRoot()
	if err != nil {
		return nil, err
	}
	home, err := homeDir()
	if err != nil {
		return nil, err
	}
	store := func(option string, fallback any) *action {
		return &action{kind: storeAction, options: []string{option}, dest: dest(option), fallback: fallback}
	}
	appendTo := func(option, metavar, help string) *action {
		return &action{kind: appendAction, options: []string{option}, dest: dest(option), metavar: metavar,
			help: help, fallback: []string{}}
	}
	return &parser{prog: "agent-decisions-install", stdout: stdout, stderr: stderr, actions: []*action{
		{kind: helpAction, options: []string{"-h", "--help"}, dest: "help", help: "show this help message and exit"},
		{kind: storeAction, options: []string{"--host"}, dest: "host", choices: []string{"codex", "claude", "both"},
			fallback: "both"},
		{kind: storeTrueAction, options: []string{"--apply"}, dest: "apply", fallback: false},
		{kind: storeAction, options: []string{"--review-mode"}, dest: "review_mode",
			choices: []string{"required", "availability"}, fallback: "required",
			help: "Required blocks unchecked actions; availability permits them with a warning."},
		appendTo("--session-review-exception", "SESSION_ID",
			"Owner-authorized review exception for one canonical session and its native workers."),
		appendTo("--clear-session-review-exception", "SESSION_ID", "Revoke an owner-authorized session review exception."),
		store("--state-root", stateDir),
		store("--install-root", installRoot),
		store("--codex-binary", CodexBinary),
		store("--codex-sock", nil),
		store("--codex-config", pathlib.Join(home, ".codex/config.toml")),
		store("--codex-mcp-json", nil),
		store("--claude-settings", pathlib.Join(home, ".claude/settings.json")),
		appendTo("--claude-json", "", ""),
		appendTo("--claude-plugin-manifest", "", ""),
	}}, nil
}

// dest returns the dest argparse derives from a long option: "--state-root" is "state_root".
func dest(option string) string {
	return strings.ReplaceAll(option[2:], "-", "_") // ASCII option strings
}

func (p *parser) optionAction(option string) *action {
	for _, a := range p.actions {
		if slices.Contains(a.options, option) {
			return a
		}
	}
	return nil
}

// parseArgs returns the parsed arguments, or the exit status after argparse printed help or an error.
//
// py: Lib/argparse.py ArgumentParser.parse_args (Python 3.15)
func (p *parser) parseArgs(argv []string) (*cliArgs, int, bool) {
	ns := namespace{}
	for _, a := range p.actions {
		if a.kind != helpAction {
			ns[a.dest] = a.fallback
		}
	}
	extras, err := p.parseKnown(argv, ns)
	if status, ok := err.(exitStatus); ok {
		return nil, int(status), true
	}
	if err != nil {
		return nil, p.error(err.Error()), true
	}
	if len(extras) > 0 {
		return nil, p.error("unrecognized arguments: " + strings.Join(extras, " ")), true
	}
	text := func(key string) string { return ns[key].(string) }
	optional := func(key string) *string {
		if value, ok := ns[key].(string); ok {
			return &value
		}
		return nil
	}
	return &cliArgs{
		host: text("host"), reviewMode: text("review_mode"), stateRoot: text("state_root"),
		installRoot: text("install_root"), codexBinary: text("codex_binary"), codexConfig: text("codex_config"),
		claudeSettings: text("claude_settings"), apply: ns["apply"].(bool),
		sessionReviewException:      ns["session_review_exception"].([]string),
		clearSessionReviewException: ns["clear_session_review_exception"].([]string),
		claudeJSON:                  ns["claude_json"].([]string),
		claudePluginManifest:        ns["claude_plugin_manifest"].([]string),
		codexSock:                   optional("codex_sock"), codexMCPJSON: optional("codex_mcp_json"),
	}, 0, false
}

// optionTuple is one interpretation of an option argument. A nil action is an unknown option. sep is ""
// for None and "", and explicit is nil for None.
type optionTuple struct {
	action   *action
	option   string
	sep      string
	explicit *string
}

// parseKnown consumes argv into ns and returns the unrecognized arguments. Every argument is an option or
// an extra: the parser has no positionals.
//
// py: Lib/argparse.py ArgumentParser._parse_known_args (Python 3.15)
func (p *parser) parseKnown(args []string, ns namespace) ([]string, error) {
	// 'O' marks an option, 'A' an argument and '-' the first "--", after which everything is an argument.
	options := map[int][]optionTuple{}
	pattern := make([]byte, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			pattern = append(pattern, '-')
			pattern = append(pattern, strings.Repeat("A", len(args)-i-1)...)
			break
		}
		if tuples := p.parseOptional(arg); tuples != nil {
			options[i] = tuples
			pattern = append(pattern, 'O')
		} else {
			pattern = append(pattern, 'A')
		}
	}

	takeAction := func(a *action, values []string) error {
		switch a.kind {
		case helpAction:
			p.printHelp(p.stdout)
			return exitStatus(0)
		case storeTrueAction:
			ns[a.dest] = true
		case storeAction, appendAction:
			if err := checkValue(a, values[0]); err != nil {
				return err
			}
			if a.kind == storeAction {
				ns[a.dest] = values[0]
			} else {
				ns[a.dest] = append(slices.Clone(ns[a.dest].([]string)), values[0])
			}
		}
		return nil
	}

	var extras []string
	consumeOptional := func(start int) (int, error) {
		tuples := options[start]
		if len(tuples) > 1 {
			matches := make([]string, len(tuples))
			for i, tuple := range tuples {
				matches[i] = tuple.option
			}
			return 0, &argumentError{msg: fmt.Sprintf("ambiguous option: %s could match %s",
				args[start], strings.Join(matches, ", "))}
		}
		a, option, sep, explicit := tuples[0].action, tuples[0].option, tuples[0].sep, tuples[0].explicit
		type call struct {
			action *action
			values []string
		}
		var calls []call
		stop := start + 1
	loop:
		for {
			takesValue := a != nil && (a.kind == storeAction || a.kind == appendAction)
			switch {
			case a == nil:
				extras = append(extras, args[start])
				return start + 1, nil
			case explicit == nil:
				count := 0
				if takesValue {
					if start+1 >= len(pattern) || pattern[start+1] != 'A' {
						return 0, &argumentError{a, "expected one argument"}
					}
					count = 1
				}
				stop = start + 1 + count
				calls = append(calls, call{a, args[start+1 : stop]})
				break loop
			case takesValue:
				calls = append(calls, call{a, []string{*explicit}})
				break loop
			case option[1] != '-' && *explicit != "":
				// A single-dash flag: the rest of the argument may hold more flags, as in -xyz.
				if sep != "" || (*explicit)[0] == '-' {
					return 0, &argumentError{a, "ignored explicit argument " + str.Repr(*explicit)}
				}
				calls = append(calls, call{a, nil})
				_, size := str.DecodeRune(*explicit)
				option = "-" + (*explicit)[:size]
				a = p.optionAction(option)
				if a == nil {
					extras = append(extras, "-"+*explicit)
					break loop
				}
				rest := (*explicit)[size:]
				switch {
				case rest == "":
					sep, explicit = "", nil
				case rest[0] == '=':
					sep, rest = "=", rest[1:]
					explicit = &rest
				default:
					sep, explicit = "", &rest
				}
			default:
				return 0, &argumentError{a, "ignored explicit argument " + str.Repr(*explicit)}
			}
		}
		for _, c := range calls {
			if err := takeAction(c.action, c.values); err != nil {
				return 0, err
			}
		}
		return stop, nil
	}

	start, maxOption := 0, -1
	for i := range options {
		maxOption = max(maxOption, i)
	}
	for start <= maxOption {
		next := start
		for next <= maxOption {
			if _, ok := options[next]; ok {
				break
			}
			next++
		}
		if _, ok := options[start]; !ok {
			extras = append(extras, args[start:next]...)
			start = next
		}
		var err error
		if start, err = consumeOptional(start); err != nil {
			return nil, err
		}
	}
	return append(extras, args[start:]...), nil
}

// parseOptional returns the interpretations of arg as an option, or nil when it is positional.
//
// py: Lib/argparse.py ArgumentParser._parse_optional (Python 3.15)
func (p *parser) parseOptional(arg string) []optionTuple {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if a := p.optionAction(arg); a != nil {
		return []optionTuple{{action: a, option: arg}}
	}
	if arg == "-" {
		return nil
	}
	if option, explicit, found := strings.Cut(arg, "="); found {
		if a := p.optionAction(option); a != nil {
			return []optionTuple{{action: a, option: option, sep: "=", explicit: &explicit}}
		}
	}
	if tuples := p.optionTuples(arg); len(tuples) > 0 {
		return tuples
	}
	if negativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return nil
	}
	return []optionTuple{{option: arg}}
}

// optionTuples returns the options that arg abbreviates and, for a single dash, the option it starts
// with.
//
// py: Lib/argparse.py ArgumentParser._get_option_tuples (Python 3.15)
func (p *parser) optionTuples(arg string) []optionTuple {
	prefix, explicitText, found := strings.Cut(arg, "=")
	sep, explicit := "", (*string)(nil)
	if found {
		sep, explicit = "=", &explicitText
	}
	short, shortExplicit := str.SliceTo(arg, 2), str.SliceFrom(arg, 2)
	var result []optionTuple
	for _, a := range p.actions {
		for _, option := range a.options {
			switch {
			case arg[1] != '-' && option == short:
				result = append(result, optionTuple{a, option, "", &shortExplicit})
			case strings.HasPrefix(option, prefix):
				result = append(result, optionTuple{a, option, sep, explicit})
			}
		}
	}
	return result
}

// py: Lib/argparse.py ArgumentParser._check_value (Python 3.15)
func checkValue(a *action, value string) error {
	if a.choices == nil || slices.Contains(a.choices, value) {
		return nil
	}
	quoted := make([]string, len(a.choices))
	for i, choice := range a.choices {
		quoted[i] = str.Repr(choice)
	}
	choices := strings.Join(quoted, ", ")
	if closest := closeMatch(value, a.choices); closest != "" {
		return &argumentError{a, fmt.Sprintf("invalid choice: %s, maybe you meant %s? (choose from %s)",
			str.Repr(value), str.Repr(closest), choices)}
	}
	return &argumentError{a, fmt.Sprintf("invalid choice: %s (choose from %s)", str.Repr(value), choices)}
}

// error prints the usage and message to stderr and returns the exit status 2.
//
// py: Lib/argparse.py ArgumentParser.error (Python 3.15)
func (p *parser) error(message string) int {
	p.printUsage(p.stderr)
	t := themeFor(p.stderr)
	pyWrite(p.stderr, fmt.Sprintf("%s: %serror:%s %s%s%s\n", p.prog, t.error, t.reset, t.message, message, t.reset))
	return 2
}

// py: Lib/argparse.py ArgumentParser.print_usage (Python 3.15)
func (p *parser) printUsage(w io.Writer) {
	f := newFormatter(p.prog, themeFor(w))
	pyWrite(w, strings.Trim(f.formatUsage(p), "\n")+"\n")
}

// py: Lib/argparse.py ArgumentParser.print_help (Python 3.15)
func (p *parser) printHelp(w io.Writer) {
	pyWrite(w, newFormatter(p.prog, themeFor(w)).formatHelp(p))
}

// theme is the argparse section of a _colorize theme. The zero theme has no colors.
type theme struct {
	usage, prog, heading                                  string
	summaryLongOption, summaryShortOption, summaryLabel   string
	longOption, shortOption, label, reset, error, message string
}

var colorTheme = theme{
	usage: "\x1b[1;34m", prog: "\x1b[1;35m", heading: "\x1b[1;34m",
	summaryLongOption: "\x1b[36m", summaryShortOption: "\x1b[32m", summaryLabel: "\x1b[33m",
	longOption: "\x1b[1;36m", shortOption: "\x1b[1;32m", label: "\x1b[1;33m",
	reset: "\x1b[0m", error: "\x1b[1;35m", message: "\x1b[35m",
}

// themeFor returns the theme for text written to w. The launcher runs python3 without -I, so
// PYTHON_COLORS applies.
//
// py: Lib/_colorize.py can_colorize (Python 3.15)
func themeFor(w io.Writer) *theme {
	switch {
	case os.Getenv("PYTHON_COLORS") == "0":
	case os.Getenv("PYTHON_COLORS") == "1":
		return &colorTheme
	case os.Getenv("NO_COLOR") != "":
	case os.Getenv("FORCE_COLOR") != "":
		return &colorTheme
	case os.Getenv("TERM") == "dumb":
	case isatty(w):
		return &colorTheme
	}
	return &theme{}
}

var ansiCode = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// decolor removes the theme's color codes. The formatted text holds no other escape sequences.
//
// py: Lib/_colorize.py decolor (Python 3.15)
func decolor(text string) string {
	return ansiCode.ReplaceAllString(text, "")
}

// formatter is argparse.HelpFormatter with its default arguments.
type formatter struct {
	prog            string
	width           int
	maxHelpPosition int
	actionMaxLength int
	theme           *theme
}

// py: Lib/argparse.py HelpFormatter.__init__ (Python 3.15)
func newFormatter(prog string, t *theme) *formatter {
	width := terminalColumns() - 2
	return &formatter{prog: prog, width: width, maxHelpPosition: min(24, max(width-20, 4)), theme: t}
}

// metavar returns the metavar of a: its metavar, its choices, or its upper-cased dest.
//
// py: Lib/argparse.py HelpFormatter._metavar_formatter (Python 3.15)
func metavar(a *action) string {
	switch {
	case a.metavar != "":
		return a.metavar
	case a.choices != nil:
		return "{" + strings.Join(a.choices, ",") + "}"
	}
	return strings.ToUpper(a.dest) // ASCII dest
}

func takesValue(a *action) bool {
	return a.kind == storeAction || a.kind == appendAction
}

// usageParts returns the usage part of each action.
//
// py: Lib/argparse.py HelpFormatter._get_actions_usage_parts (Python 3.15)
func (f *formatter) usageParts(p *parser) []string {
	t := f.theme
	var parts []string
	for _, a := range p.actions {
		option := a.options[0]
		color := t.summaryShortOption
		if len(option) > 2 {
			color = t.summaryLongOption
		}
		part := color + option + t.reset
		if takesValue(a) {
			part = color + option + " " + t.summaryLabel + metavar(a) + t.reset
		}
		parts = append(parts, "["+part+"]")
	}
	return parts
}

// formatUsage returns the usage block, which ends with a blank line.
//
// py: Lib/argparse.py HelpFormatter._format_usage (Python 3.15)
func (f *formatter) formatUsage(p *parser) string {
	t := f.theme
	const prefix = "usage: "
	prog := f.prog
	parts := f.usageParts(p)
	usage := strings.Join(append([]string{prog}, parts...), " ")
	textWidth := f.width
	if len(prefix)+len(decolor(usage)) > textWidth {
		getLines := func(parts []string, indent string, withPrefix bool) []string {
			var lines, line []string
			lineLen := len(indent) - 1
			if withPrefix {
				lineLen = len(prefix) - 1
			}
			for _, part := range parts {
				partLen := len(decolor(part))
				if lineLen+1+partLen > textWidth && len(line) > 0 {
					lines = append(lines, indent+strings.Join(line, " "))
					line, lineLen = nil, len(indent)-1
				}
				line = append(line, part)
				lineLen += partLen + 1
			}
			if len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
			}
			if withPrefix {
				lines[0] = lines[0][len(indent):]
			}
			return lines
		}
		var lines []string
		if float64(len(prefix)+len(prog)) <= 0.75*float64(textWidth) {
			indent := strings.Repeat(" ", len(prefix)+len(prog)+1)
			lines = getLines(append([]string{prog}, parts...), indent, true)
		} else {
			lines = append([]string{prog}, getLines(parts, strings.Repeat(" ", len(prefix)), false)...)
		}
		usage = strings.Join(lines, "\n")
	}
	usage = t.prog + prog + t.reset + strings.TrimPrefix(usage, prog)
	return t.usage + prefix + t.reset + usage + "\n\n"
}

// py: Lib/argparse.py HelpFormatter._format_action_invocation (Python 3.15)
func (f *formatter) invocation(a *action) string {
	t := f.theme
	var options []string
	for _, option := range a.options {
		color := t.shortOption
		if len(option) > 2 {
			color = t.longOption
		}
		options = append(options, color+option+t.reset)
	}
	if !takesValue(a) {
		return strings.Join(options, ", ")
	}
	return strings.Join(options, ", ") + " " + t.label + metavar(a) + t.reset
}

var longBreak = regexp.MustCompile(`\n\n\n+`)

// formatHelp returns the help text of p: the usage and the options section.
//
// py: Lib/argparse.py ArgumentParser.format_help, HelpFormatter.format_help (Python 3.15)
func (f *formatter) formatHelp(p *parser) string {
	t := f.theme
	for _, a := range p.actions {
		f.actionMaxLength = max(f.actionMaxLength, len(decolor(f.invocation(a)))+2) // section indent 2
	}
	var body strings.Builder
	for _, a := range p.actions {
		body.WriteString(f.formatAction(a, 2))
	}
	help := f.formatUsage(p) + "\n" + t.heading + "options:" + t.reset + "\n" + body.String() + "\n"
	help = longBreak.ReplaceAllString(help, "\n\n")
	return strings.Trim(help, "\n") + "\n"
}

// py: Lib/argparse.py HelpFormatter._format_action (Python 3.15)
func (f *formatter) formatAction(a *action, indent int) string {
	helpPosition := min(f.actionMaxLength+2, f.maxHelpPosition)
	helpWidth := max(f.width-helpPosition, 11)
	actionWidth := helpPosition - indent - 2
	header := f.invocation(a)
	plain := decolor(header)
	var out strings.Builder
	indentFirst := 0
	switch {
	case a.help == "":
		out.WriteString(strings.Repeat(" ", indent) + header + "\n")
	case len(plain) <= actionWidth:
		padded := strings.Repeat(" ", indent) + plain + strings.Repeat(" ", actionWidth-len(plain)) + "  "
		out.WriteString(strings.ReplaceAll(padded, plain, header))
	default:
		out.WriteString(strings.Repeat(" ", indent) + header + "\n")
		indentFirst = helpPosition
	}
	if a.help != "" {
		lines := wrap(a.help, helpWidth)
		out.WriteString(strings.Repeat(" ", indentFirst) + lines[0] + "\n")
		for _, line := range lines[1:] {
			out.WriteString(strings.Repeat(" ", helpPosition) + line + "\n")
		}
	}
	return out.String()
}

// hyphenated is the hyphenated-word branch of TextWrapper.wordsep_re for ASCII text: a word breaks after
// a hyphen that two letters precede and a letter follows.
var hyphenated = regexp.MustCompile(`[A-Za-z]{2}-[A-Za-z]`)

// wrapChunks splits text as TextWrapper._split_chunks does with break_on_hyphens: spaces and words, and
// hyphenated words after their hyphens.
// ponytail: ASCII help text with single spaces and no em-dashes; the full wordsep_re is not ported.
func wrapChunks(text string) []string {
	var chunks []string
	for i, word := range strings.Split(text, " ") {
		if i > 0 {
			chunks = append(chunks, " ")
		}
		for {
			match := hyphenated.FindStringIndex(word)
			if match == nil {
				break
			}
			chunks, word = append(chunks, word[:match[0]+3]), word[match[0]+3:]
		}
		chunks = append(chunks, word)
	}
	return chunks
}

// wrap returns textwrap.wrap(text, width) for the ASCII help texts of the parser.
//
// py: Lib/textwrap.py TextWrapper._wrap_chunks, _handle_long_word (Python 3.15)
func wrap(text string, width int) []string {
	chunks := wrapChunks(text)
	var lines []string
	for len(chunks) > 0 {
		if chunks[0] == " " && len(lines) > 0 {
			chunks = chunks[1:]
		}
		var line []string
		length := 0
		for len(chunks) > 0 && length+len(chunks[0]) <= width {
			line, length, chunks = append(line, chunks[0]), length+len(chunks[0]), chunks[1:]
		}
		if len(chunks) > 0 && len(chunks[0]) > width {
			spaceLeft := max(width-length, 1)
			chunk, end := chunks[0], spaceLeft
			if len(chunk) > spaceLeft {
				if hyphen := strings.LastIndex(chunk[:spaceLeft], "-"); hyphen > 0 && strings.Trim(chunk[:hyphen], "-") != "" {
					end = hyphen + 1
				}
			}
			line, chunks[0] = append(line, chunk[:end]), chunk[end:]
		}
		if len(line) > 0 && line[len(line)-1] == " " {
			line = line[:len(line)-1]
		}
		if len(line) > 0 {
			lines = append(lines, strings.Join(line, ""))
		}
	}
	return lines
}

// closeMatch returns difflib.get_close_matches(word, possibilities, 1)[0], or "" when nothing scores at
// least 0.6.
//
// py: Lib/difflib.py get_close_matches (Python 3.15)
func closeMatch(word string, possibilities []string) string {
	const cutoff = 0.6
	b := codePoints(word)
	best, bestScore := "", 0.0
	for _, x := range possibilities {
		a := codePoints(x)
		if realQuickRatio(a, b) < cutoff || quickRatio(a, b) < cutoff {
			continue
		}
		score := ratio(a, b)
		if score >= cutoff && (best == "" || score > bestScore || score == bestScore && x > best) {
			best, bestScore = x, score // heapq.nlargest(1) on (score, x)
		}
	}
	return best
}

func codePoints(s string) []rune {
	var runes []rune
	for s != "" {
		r, size := str.DecodeRune(s)
		runes, s = append(runes, r), s[size:]
	}
	return runes
}

// py: Lib/difflib.py _calculate_ratio (Python 3.15)
func calculateRatio(matches, length int) float64 {
	if length == 0 {
		return 1.0
	}
	return 2.0 * float64(matches) / float64(length)
}

// py: Lib/difflib.py SequenceMatcher.real_quick_ratio (Python 3.15)
func realQuickRatio(a, b []rune) float64 {
	return calculateRatio(min(len(a), len(b)), len(a)+len(b))
}

// py: Lib/difflib.py SequenceMatcher.quick_ratio (Python 3.15)
func quickRatio(a, b []rune) float64 {
	avail := map[rune]int{}
	for _, r := range b {
		avail[r]++
	}
	matches := 0
	for _, r := range a {
		if avail[r] > 0 {
			matches++
		}
		avail[r]--
	}
	return calculateRatio(matches, len(a)+len(b))
}

// ratio returns SequenceMatcher(None, a, b).ratio(), with the autojunk heuristic.
//
// py: Lib/difflib.py SequenceMatcher.ratio, get_matching_blocks, find_longest_match (Python 3.15)
func ratio(a, b []rune) float64 {
	b2j := map[rune][]int{}
	for j, r := range b {
		b2j[r] = append(b2j[r], j)
	}
	if n := len(b); n >= 200 {
		for r, indices := range b2j {
			if len(indices) > n/100+1 {
				delete(b2j, r) // a popular element
			}
		}
	}
	longest := func(alo, ahi, blo, bhi int) (int, int, int) {
		besti, bestj, bestsize := alo, blo, 0
		j2len := map[int]int{}
		for i := alo; i < ahi; i++ {
			next := map[int]int{}
			for _, j := range b2j[a[i]] {
				if j < blo {
					continue
				}
				if j >= bhi {
					break
				}
				k := j2len[j-1] + 1
				next[j] = k
				if k > bestsize {
					besti, bestj, bestsize = i-k+1, j-k+1, k
				}
			}
			j2len = next
		}
		// The junk set is empty, so only the extension over popular elements applies.
		for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
			besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
		}
		for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
			bestsize++
		}
		return besti, bestj, bestsize
	}
	matches := 0
	queue := [][4]int{{0, len(a), 0, len(b)}}
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		i, j, k := longest(alo, ahi, blo, bhi)
		if k == 0 {
			continue
		}
		matches += k
		if alo < i && blo < j {
			queue = append(queue, [4]int{alo, i, blo, j})
		}
		if i+k < ahi && j+k < bhi {
			queue = append(queue, [4]int{i + k, ahi, j + k, bhi})
		}
	}
	return calculateRatio(matches, len(a)+len(b))
}

// terminalColumns ports shutil.get_terminal_size().columns: $COLUMNS when it is a positive int, else the
// width of the terminal on file descriptor 1, else 80.
//
// py: Lib/shutil.py get_terminal_size (Python 3.15)
func terminalColumns() int {
	if columns, ok := pyInt(os.Getenv("COLUMNS")); ok && columns > 0 {
		return columns
	}
	if columns, ok := winsizeColumns(1); ok && columns > 0 {
		return columns
	}
	return 80
}

// pyInt ports int(s) for a str s, saturating far beyond any terminal width.
// ponytail: only ASCII digits; int() also accepts other Unicode decimal digits.
func pyInt(s string) (int, bool) {
	s = str.Strip(s)
	negative := false
	if s != "" && (s[0] == '-' || s[0] == '+') {
		negative, s = s[0] == '-', s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	n := 0
	for i := range len(s) {
		switch c := s[i]; {
		case c == '_':
		case c >= '0' && c <= '9':
			n = min(n*10+int(c-'0'), 1<<40)
		default:
			return 0, false
		}
	}
	if negative {
		n = -n
	}
	return n, true
}

// winsizeColumns returns the terminal width on fd, as os.get_terminal_size(fd) does.
func winsizeColumns(fd uintptr) (int, bool) {
	var size struct{ rows, columns, xpixels, ypixels uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	return int(size.columns), errno == 0
}

// isatty ports os.isatty(file.fileno()) for a writer: false for a writer without a file descriptor.
func isatty(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	_, isTerminal := winsizeColumns(file.Fd())
	return isTerminal
}
