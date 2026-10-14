package isolation

// The subset of CPython 3.15's argparse that the agent-decisions-isolate parser uses: help, store and
// store_true optionals, one subparsers positional, abbreviated options, the errors with their
// close-match suggestions, and the usage and help text with its wrapping and colors.

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	pyre "github.com/rexbrahh/make-better-decisions/internal/py/re"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

type actionKind int

const (
	helpAction      actionKind = iota // action="help", nargs=0
	storeAction                       // the default action, nargs=None
	storeTrueAction                   // action="store_true", nargs=0
	parsersAction                     // add_subparsers(), nargs=PARSER
)

// action is an argparse Action. An action without options is positional.
type action struct {
	kind       actionKind
	options    []string
	dest       string   // "" is SUPPRESS
	choices    []string // nil is None
	required   bool
	help       string  // "" is None
	parser     *parser // the parser of the only subcommand of a parsersAction
	choiceHelp string  // the help of that subcommand
}

type parser struct {
	prog    string
	actions []*action
}

// streams are the files that argparse prints to: sys.stdout and sys.stderr.
type streams struct {
	stdout, stderr io.Writer
}

// systemExit is the SystemExit that argparse raises after it prints help or an error.
type systemExit struct {
	code int
}

func (e *systemExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// argumentError is argparse.ArgumentError. A nil action has no argument name.
type argumentError struct {
	action *action
	msg    string
}

func (e *argumentError) Error() string {
	if e.action == nil {
		return e.msg
	}
	return "argument " + actionName(e.action) + ": " + e.msg
}

// namespace is an argparse.Namespace: dest to value, where nil is None.
type namespace map[string]any

const unrecognizedArgs = "_unrecognized_args"

// negativeNumber is _negative_number_matcher. The parsers here have no option that looks like a negative
// number, so an argument that matches it is positional.
var negativeNumber = regexp.MustCompile(`\A-\.?` + pyre.Digit)

// isolateParser builds the parser of main_isolate.
//
// py: agent_decisions/isolation.py main_isolate
func isolateParser() *parser {
	hosts := []string{"claude", "codex"}
	run := &parser{prog: "agent-decisions-isolate exec", actions: []*action{
		helpArgument(),
		{kind: storeAction, options: []string{"--host"}, dest: "host", choices: hosts, required: true},
		{kind: storeAction, options: []string{"--command"}, dest: "command", required: true},
		{kind: storeTrueAction, options: []string{"--read-only"}, dest: "read_only"},
	}}
	return &parser{prog: "agent-decisions-isolate", actions: []*action{
		helpArgument(),
		{kind: parsersAction, dest: "mode", choices: []string{"exec"}, parser: run,
			choiceHelp: "run an opaque command from the signed host spec"},
		{kind: storeAction, options: []string{"--server"}, dest: "server"},
		{kind: storeAction, options: []string{"--host"}, dest: "host", choices: hosts},
	}}
}

func helpArgument() *action {
	return &action{kind: helpAction, options: []string{"-h", "--help"}, help: "show this help message and exit"}
}

// optionAction returns the action of an option string, or nil.
func (p *parser) optionAction(option string) *action {
	for _, a := range p.actions {
		if slices.Contains(a.options, option) {
			return a
		}
	}
	return nil
}

// defaultValue returns the namespace default of a: None, or False for store_true.
func (a *action) defaultValue() any {
	if a.kind == storeTrueAction {
		return false
	}
	return nil
}

// py: Lib/argparse.py _get_action_name (Python 3.15)
func actionName(a *action) string {
	if len(a.options) > 0 {
		return strings.Join(a.options, "/")
	}
	return a.dest
}

// py: Lib/argparse.py ArgumentParser.parse_args (Python 3.15)
func (p *parser) parseArgs(args []string, out *streams) (namespace, error) {
	ns, extras, err := p.parseKnownArgs(args, out)
	if err != nil {
		return nil, err
	}
	if len(extras) > 0 {
		return nil, p.error("unrecognized arguments: "+strings.Join(extras, " "), out)
	}
	return ns, nil
}

// py: Lib/argparse.py ArgumentParser._parse_known_args2 (Python 3.15)
func (p *parser) parseKnownArgs(args []string, out *streams) (namespace, []string, error) {
	ns := namespace{}
	for _, a := range p.actions {
		if a.dest != "" {
			ns[a.dest] = a.defaultValue()
		}
	}
	extras, err := p.parseKnown(args, ns, out)
	if err != nil {
		if argErr, ok := err.(*argumentError); ok {
			return nil, nil, p.error(argErr.Error(), out)
		}
		return nil, nil, err
	}
	if unrecognized, ok := ns[unrecognizedArgs]; ok {
		extras = append(extras, unrecognized.([]string)...)
		delete(ns, unrecognizedArgs)
	}
	return ns, extras, nil
}

// optionTuple is one interpretation of an option argument. A nil action is an unknown option. sep is ""
// for None and "", and explicit is nil for None.
type optionTuple struct {
	action   *action
	option   string
	sep      string
	explicit *string
}

// py: Lib/argparse.py ArgumentParser._parse_known_args (Python 3.15)
func (p *parser) parseKnown(args []string, ns namespace, out *streams) ([]string, error) {
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

	seen := map[*action]bool{}
	takeAction := func(a *action, values []string) error {
		seen[a] = true
		switch a.kind {
		case helpAction:
			p.printHelp(out.stdout)
			return &systemExit{0}
		case storeTrueAction:
			ns[a.dest] = true
		case storeAction:
			if err := checkValue(a, values[0]); err != nil {
				return err
			}
			ns[a.dest] = values[0]
		case parsersAction:
			if err := checkValue(a, values[0]); err != nil {
				return err
			}
			ns[a.dest] = values[0]
			subNamespace, subExtras, err := a.parser.parseKnownArgs(values[1:], out)
			if err != nil {
				return err
			}
			for key, value := range subNamespace {
				ns[key] = value
			}
			if len(subExtras) > 0 {
				previous, _ := ns[unrecognizedArgs].([]string)
				ns[unrecognizedArgs] = append(previous, subExtras...)
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
			switch {
			case a == nil:
				extras = append(extras, args[start])
				return start + 1, nil
			case explicit == nil:
				count := 0
				if a.kind == storeAction {
					if start+1 >= len(pattern) || pattern[start+1] != 'A' {
						return 0, &argumentError{a, "expected one argument"}
					}
					count = 1
				}
				stop = start + 1 + count
				calls = append(calls, call{a, args[start+1 : stop]})
				break loop
			case a.kind == storeAction:
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

	var positionals []*action
	for _, a := range p.actions {
		if len(a.options) == 0 {
			positionals = append(positionals, a)
		}
	}
	consumePositionals := func(start int) (int, error) {
		// The only positional is a parsersAction, whose nargs pattern '(-*A[-AO]*)' takes everything left
		// once an argument follows the leading "--".
		var counts []int
		if len(positionals) > 0 {
			selected := strings.TrimLeft(string(pattern[start:]), "-")
			if strings.HasPrefix(selected, "A") {
				counts = []int{len(pattern) - start}
			}
		}
		for i, count := range counts {
			values := slices.Clone(args[start : start+count])
			if pattern[start] == '-' {
				values = slices.Delete(values, 0, 1) // args.remove('--')
			}
			start += count
			if err := takeAction(positionals[i], values); err != nil {
				return 0, err
			}
		}
		positionals = positionals[len(counts):]
		return start, nil
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
		if start != next {
			end, err := consumePositionals(start)
			if err != nil {
				return nil, err
			}
			if end > start {
				start = end
				continue
			}
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
	stop, err := consumePositionals(start)
	if err != nil {
		return nil, err
	}
	extras = append(extras, args[stop:]...)

	var required []string
	for _, a := range p.actions {
		if !seen[a] && a.required {
			required = append(required, actionName(a))
		}
	}
	if len(required) > 0 {
		return nil, &argumentError{msg: "the following arguments are required: " + strings.Join(required, ", ")}
	}
	return extras, nil
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

// py: Lib/argparse.py ArgumentParser.error (Python 3.15)
func (p *parser) error(message string, out *streams) *systemExit {
	p.printUsage(out.stderr)
	t := themeFor(out.stderr)
	pyWrite(out.stderr, fmt.Sprintf("%s: %serror:%s %s%s%s\n", p.prog, t.error, t.reset, t.message, message, t.reset))
	return &systemExit{2}
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
	usage, prog, heading                                               string
	summaryLongOption, summaryShortOption, summaryLabel, summaryAction string
	longOption, shortOption, label, action, reset, error, message      string
}

var colorTheme = theme{
	usage: "\x1b[1;34m", prog: "\x1b[1;35m", heading: "\x1b[1;34m",
	summaryLongOption: "\x1b[36m", summaryShortOption: "\x1b[32m", summaryLabel: "\x1b[33m", summaryAction: "\x1b[32m",
	longOption: "\x1b[1;36m", shortOption: "\x1b[1;32m", label: "\x1b[1;33m", action: "\x1b[1;32m",
	reset: "\x1b[0m", error: "\x1b[1;35m", message: "\x1b[35m",
}

// themeFor returns the theme for text written to w.
//
// py: Lib/_colorize.py can_colorize (Python 3.15)
func themeFor(w io.Writer) *theme {
	// python3 -I ignores PYTHON_COLORS.
	switch {
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

// metavar returns the metavar of a: its choices, or its dest (upper-cased for an option).
//
// py: Lib/argparse.py HelpFormatter._metavar_formatter (Python 3.15)
func metavar(a *action) string {
	switch {
	case a.choices != nil:
		return "{" + strings.Join(a.choices, ",") + "}"
	case len(a.options) > 0:
		return strings.ToUpper(a.dest) // ASCII dest
	}
	return a.dest
}

// py: Lib/argparse.py HelpFormatter._format_args (Python 3.15)
func formatArgs(a *action) string {
	if a.kind == parsersAction {
		return metavar(a) + " ..."
	}
	return metavar(a)
}

func takesValue(a *action) bool {
	return a.kind == storeAction || a.kind == parsersAction
}

// usageParts returns the usage parts of the actions, optionals first, and the index of the first
// positional.
//
// py: Lib/argparse.py HelpFormatter._get_actions_usage_parts (Python 3.15)
func (f *formatter) usageParts(p *parser) ([]string, int) {
	t := f.theme
	var optionals, positionals []string
	for _, a := range p.actions {
		if len(a.options) == 0 {
			positionals = append(positionals, t.summaryAction+formatArgs(a)+t.reset)
			continue
		}
		option := a.options[0]
		color := t.summaryShortOption
		if len(option) > 2 {
			color = t.summaryLongOption
		}
		part := color + option + t.reset
		if takesValue(a) {
			part = color + option + " " + t.summaryLabel + formatArgs(a) + t.reset
		}
		if !a.required {
			part = "[" + part + "]"
		}
		optionals = append(optionals, part)
	}
	return append(optionals, positionals...), len(optionals)
}

// formatUsage returns the usage block, which ends with a blank line.
//
// py: Lib/argparse.py HelpFormatter._format_usage (Python 3.15)
func (f *formatter) formatUsage(p *parser) string {
	t := f.theme
	const prefix = "usage: "
	prog := f.prog
	parts, positionalStart := f.usageParts(p)
	usage := strings.Join(append([]string{prog}, parts...), " ")
	textWidth := f.width
	if len(prefix)+len(decolor(usage)) > textWidth {
		optionals, positionals := parts[:positionalStart], parts[positionalStart:]
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
			switch {
			case len(optionals) > 0:
				lines = getLines(append([]string{prog}, optionals...), indent, true)
				lines = append(lines, getLines(positionals, indent, false)...)
			case len(positionals) > 0:
				lines = getLines(append([]string{prog}, positionals...), indent, true)
			default:
				lines = []string{prog}
			}
		} else {
			indent := strings.Repeat(" ", len(prefix))
			lines = getLines(parts, indent, false)
			if len(lines) > 1 {
				lines = append(getLines(optionals, indent, false), getLines(positionals, indent, false)...)
			}
			lines = append([]string{prog}, lines...)
		}
		usage = strings.Join(lines, "\n")
	}
	usage = t.prog + prog + t.reset + strings.TrimPrefix(usage, prog)
	return t.usage + prefix + t.reset + usage + "\n\n"
}

// subactions returns the choice pseudo-actions of a parsersAction, which list each subcommand.
//
// py: Lib/argparse.py _SubParsersAction._get_subactions (Python 3.15)
func subactions(a *action) []*action {
	if a.kind != parsersAction {
		return nil
	}
	return []*action{{kind: storeAction, dest: a.choices[0], help: a.choiceHelp}}
}

// py: Lib/argparse.py HelpFormatter._format_action_invocation (Python 3.15)
func (f *formatter) invocation(a *action) string {
	t := f.theme
	if len(a.options) == 0 {
		return t.action + metavar(a) + t.reset
	}
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
	return strings.Join(options, ", ") + " " + t.label + formatArgs(a) + t.reset
}

var longBreak = regexp.MustCompile(`\n\n\n+`)

// formatHelp returns the help text of p.
//
// py: Lib/argparse.py ArgumentParser.format_help, HelpFormatter.format_help (Python 3.15)
func (f *formatter) formatHelp(p *parser) string {
	t := f.theme
	var positionals, optionals []*action
	for _, a := range p.actions {
		if len(a.options) == 0 {
			positionals = append(positionals, a)
		} else {
			optionals = append(optionals, a)
		}
	}
	sections := []struct {
		heading string
		actions []*action
	}{{"positional arguments", positionals}, {"options", optionals}}
	// HelpFormatter.add_argument: every section is indented by 2, and subactions by 2 more.
	for _, section := range sections {
		for _, a := range section.actions {
			f.actionMaxLength = max(f.actionMaxLength, len(decolor(f.invocation(a)))+2)
			for _, sub := range subactions(a) {
				f.actionMaxLength = max(f.actionMaxLength, len(decolor(f.invocation(sub)))+4)
			}
		}
	}
	help := f.formatUsage(p)
	for _, section := range sections {
		var body strings.Builder
		for _, a := range section.actions {
			body.WriteString(f.formatAction(a, 2))
		}
		if body.Len() > 0 {
			help += "\n" + t.heading + section.heading + ":" + t.reset + "\n" + body.String() + "\n"
		}
	}
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
	for _, sub := range subactions(a) {
		out.WriteString(f.formatAction(sub, indent+2))
	}
	return out.String()
}

// wrap ports textwrap.wrap(text, width) for ASCII text of single-spaced words.
// ponytail: the help strings here have no word longer than 11, the least width argparse passes, so the
// long-word breaking of textwrap is not ported.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && len(line)+1+len(word) > width {
			lines, line = append(lines, line), ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	return append(lines, line)
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
