// Package argparse ports the subset of CPython 3.15's argparse that the agent-decisions commands use: help,
// store, store_true and append optionals, one subparsers positional, required arguments, abbreviated
// options, the errors with their close-match suggestions, and the usage and help text with its wrapping
// and colors.
//
// A Parser prints as ArgumentParser does and returns an *Exit where argparse raises SystemExit.
package argparse

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	pyre "github.com/rexbrahh/make-better-decisions/internal/py/re"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Kind is the action class of an Action, with the nargs that argparse gives it.
type Kind int

const (
	Help      Kind = iota // action="help", nargs=0
	Store                 // the default action, nargs=None
	StoreTrue             // action="store_true", nargs=0
	Append                // action="append", nargs=None
	Parsers               // add_subparsers(), nargs=PARSER
)

// Action is an argparse Action. An action without options is positional: a Store positional has
// nargs=None and is always required, and a Parsers positional is the subcommand.
type Action struct {
	Kind     Kind
	Options  []string
	Dest     string   // "" is SUPPRESS
	Choices  []string // nil is None
	Required bool
	Metavar  string // "" is None
	Help     string // "" is None
	// Default is the namespace value before parsing: nil (None), a string or a []string. A StoreTrue
	// action with a nil Default defaults to false, as store_true does.
	Default any
	// Parser and ChoiceHelp are the parser and help of the only subcommand of a Parsers action, which is
	// named Choices[0].
	Parser     *Parser
	ChoiceHelp string
}

// HelpAction returns the -h/--help action that ArgumentParser adds with add_help=True.
func HelpAction() *Action {
	return &Action{Kind: Help, Options: []string{"-h", "--help"}, Help: "show this help message and exit"}
}

// Parser is an ArgumentParser. Stdout and Stderr are sys.stdout and sys.stderr; a subcommand parser prints
// to those of its parent. IgnoreEnvironment is sys.flags.ignore_environment (python3 -I or -E), which
// makes the colors ignore PYTHON_COLORS.
type Parser struct {
	Prog              string
	Actions           []*Action
	Stdout, Stderr    io.Writer
	IgnoreEnvironment bool
}

// Namespace is an argparse.Namespace: dest to value, where nil is None.
type Namespace map[string]any

// Exit is the SystemExit that argparse raises after it prints help or an error.
type Exit struct {
	Code int
}

func (e *Exit) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// argumentError is argparse.ArgumentError. A nil action has no argument name.
type argumentError struct {
	action *Action
	msg    string
}

func (e *argumentError) Error() string {
	if e.action == nil {
		return e.msg
	}
	return "argument " + actionName(e.action) + ": " + e.msg
}

const unrecognizedArgs = "_unrecognized_args"

// negativeNumber is _negative_number_matcher. No parser here has an option that looks like a negative
// number, so an argument that matches it is positional.
var negativeNumber = regexp.MustCompile(`\A-\.?` + pyre.Digit)

// optionAction returns the action of an option string, or nil.
func (p *Parser) optionAction(option string) *Action {
	for _, a := range p.Actions {
		if slices.Contains(a.Options, option) {
			return a
		}
	}
	return nil
}

// defaultValue returns the namespace default of a.
func (a *Action) defaultValue() any {
	if a.Kind == StoreTrue && a.Default == nil {
		return false
	}
	return a.Default
}

// required reports whether a is required: a positional Store action always is, as add_argument makes
// every positional of nargs=None required.
//
// py: Lib/argparse.py _ActionsContainer._get_positional_kwargs (Python 3.15)
func (a *Action) required() bool {
	return a.Required || len(a.Options) == 0 && a.Kind == Store
}

// py: Lib/argparse.py _get_action_name (Python 3.15)
func actionName(a *Action) string {
	switch {
	case len(a.Options) > 0:
		return strings.Join(a.Options, "/")
	case a.Metavar != "":
		return a.Metavar
	case a.Dest != "":
		return a.Dest
	}
	return "{" + strings.Join(a.Choices, ",") + "}"
}

// matchArgumentsPartial returns the argument counts of the longest prefix of actions whose nargs patterns
// match the start of pattern.
//
// py: Lib/argparse.py ArgumentParser._match_arguments_partial, _get_nargs_pattern (Python 3.15)
func matchArgumentsPartial(actions []*Action, pattern string) []int {
	for i := len(actions); i > 0; i-- {
		var expression strings.Builder
		expression.WriteString(`\A`)
		for _, a := range actions[:i] {
			if a.Kind == Parsers {
				expression.WriteString(`(-*A[-AO]*)`)
			} else {
				expression.WriteString(`(-*A-*)`) // a positional Store, nargs=None
			}
		}
		// Go's regexp picks the submatches that a backtracking matcher such as Python's re picks.
		match := regexp.MustCompile(expression.String()).FindStringSubmatchIndex(pattern)
		if match == nil {
			continue
		}
		counts := make([]int, i)
		for j := range counts {
			counts[j] = match[2*j+3] - match[2*j+2]
		}
		if end := match[1]; end < len(pattern) && pattern[end] == 'O' {
			for len(counts) > 0 && counts[len(counts)-1] == 0 {
				counts = counts[:len(counts)-1]
			}
		}
		return counts
	}
	return nil
}

// ParseArgs returns the namespace of args, or the *Exit after it printed help or an error.
//
// py: Lib/argparse.py ArgumentParser.parse_args (Python 3.15)
func (p *Parser) ParseArgs(args []string) (Namespace, error) {
	ns, extras, err := p.parseKnownArgs(args)
	if err != nil {
		return nil, err
	}
	if len(extras) > 0 {
		return nil, p.Error("unrecognized arguments: " + strings.Join(extras, " "))
	}
	return ns, nil
}

// py: Lib/argparse.py ArgumentParser._parse_known_args2 (Python 3.15)
func (p *Parser) parseKnownArgs(args []string) (Namespace, []string, error) {
	ns := Namespace{}
	for _, a := range p.Actions {
		if a.Dest != "" {
			ns[a.Dest] = a.defaultValue()
		}
	}
	extras, err := p.parseKnown(args, ns)
	if err != nil {
		if argErr, ok := err.(*argumentError); ok {
			return nil, nil, p.Error(argErr.Error())
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
	action   *Action
	option   string
	sep      string
	explicit *string
}

// py: Lib/argparse.py ArgumentParser._parse_known_args (Python 3.15)
func (p *Parser) parseKnown(args []string, ns Namespace) ([]string, error) {
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

	seen := map[*Action]bool{}
	takeAction := func(a *Action, values []string) error {
		seen[a] = true
		switch a.Kind {
		case Help:
			p.printHelp(p.Stdout)
			return &Exit{0}
		case StoreTrue:
			ns[a.Dest] = true
		case Store, Append:
			if err := checkValue(a, values[0]); err != nil {
				return err
			}
			if a.Kind == Store {
				ns[a.Dest] = values[0]
			} else {
				ns[a.Dest] = append(slices.Clone(ns[a.Dest].([]string)), values[0])
			}
		case Parsers:
			if err := checkValue(a, values[0]); err != nil {
				return err
			}
			ns[a.Dest] = values[0]
			sub := *a.Parser
			sub.Stdout, sub.Stderr, sub.IgnoreEnvironment = p.Stdout, p.Stderr, p.IgnoreEnvironment
			subNamespace, subExtras, err := sub.parseKnownArgs(values[1:])
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
			action *Action
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
				if takesValue(a) {
					if start+1 >= len(pattern) || pattern[start+1] != 'A' {
						return 0, &argumentError{a, "expected one argument"}
					}
					count = 1
				}
				stop = start + 1 + count
				calls = append(calls, call{a, args[start+1 : stop]})
				break loop
			case takesValue(a):
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

	var positionals []*Action
	for _, a := range p.Actions {
		if len(a.Options) == 0 {
			positionals = append(positionals, a)
		}
	}
	consumePositionals := func(start int) (int, error) {
		counts := matchArgumentsPartial(positionals, string(pattern[start:]))
		for i, count := range counts {
			values := slices.Clone(args[start : start+count])
			switch {
			case positionals[i].Kind == Parsers:
				if pattern[start] == '-' {
					values = slices.Delete(values, 0, 1) // args.remove('--')
				}
			case strings.IndexByte(string(pattern[start:start+count]), '-') >= 0:
				values = slices.Delete(values, slices.Index(values, "--"), slices.Index(values, "--")+1)
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
	for _, a := range p.Actions {
		if !seen[a] && a.required() {
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
func (p *Parser) parseOptional(arg string) []optionTuple {
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
func (p *Parser) optionTuples(arg string) []optionTuple {
	prefix, explicitText, found := strings.Cut(arg, "=")
	sep, explicit := "", (*string)(nil)
	if found {
		sep, explicit = "=", &explicitText
	}
	short, shortExplicit := str.SliceTo(arg, 2), str.SliceFrom(arg, 2)
	var result []optionTuple
	for _, a := range p.Actions {
		for _, option := range a.Options {
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
func checkValue(a *Action, value string) error {
	if a.Choices == nil || slices.Contains(a.Choices, value) {
		return nil
	}
	quoted := make([]string, len(a.Choices))
	for i, choice := range a.Choices {
		quoted[i] = str.Repr(choice)
	}
	choices := strings.Join(quoted, ", ")
	if closest := closeMatch(value, a.Choices); closest != "" {
		return &argumentError{a, fmt.Sprintf("invalid choice: %s, maybe you meant %s? (choose from %s)",
			str.Repr(value), str.Repr(closest), choices)}
	}
	return &argumentError{a, fmt.Sprintf("invalid choice: %s (choose from %s)", str.Repr(value), choices)}
}

// Error prints the usage and message to Stderr and returns the SystemExit of status 2.
//
// py: Lib/argparse.py ArgumentParser.error (Python 3.15)
func (p *Parser) Error(message string) *Exit {
	p.printUsage(p.Stderr)
	t := p.themeFor(p.Stderr)
	write(p.Stderr, fmt.Sprintf("%s: %serror:%s %s%s%s\n", p.Prog, t.error, t.reset, t.message, message, t.reset))
	return &Exit{2}
}

// py: Lib/argparse.py ArgumentParser.print_usage (Python 3.15)
func (p *Parser) printUsage(w io.Writer) {
	f := newFormatter(p.Prog, p.themeFor(w))
	write(w, strings.Trim(f.formatUsage(p), "\n")+"\n")
}

// py: Lib/argparse.py ArgumentParser.print_help (Python 3.15)
func (p *Parser) printHelp(w io.Writer) {
	write(w, newFormatter(p.Prog, p.themeFor(w)).formatHelp(p))
}

// write ports _print_message: it writes s with the backslashreplace error handler of sys.stderr and
// ignores write errors.
func write(w io.Writer, s string) {
	io.WriteString(w, pyerr.EncodeBackslashReplace(s))
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
func (p *Parser) themeFor(w io.Writer) *theme {
	switch {
	case !p.IgnoreEnvironment && os.Getenv("PYTHON_COLORS") == "0":
	case !p.IgnoreEnvironment && os.Getenv("PYTHON_COLORS") == "1":
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

// metavar returns the metavar of a: its metavar, its choices, or its dest (upper-cased for an option).
//
// py: Lib/argparse.py HelpFormatter._metavar_formatter (Python 3.15)
func metavar(a *Action) string {
	switch {
	case a.Metavar != "":
		return a.Metavar
	case a.Choices != nil:
		return "{" + strings.Join(a.Choices, ",") + "}"
	case len(a.Options) > 0:
		return strings.ToUpper(a.Dest) // ASCII dest
	}
	return a.Dest
}

// py: Lib/argparse.py HelpFormatter._format_args (Python 3.15)
func formatArgs(a *Action) string {
	if a.Kind == Parsers {
		return metavar(a) + " ..."
	}
	return metavar(a)
}

func takesValue(a *Action) bool {
	return a.Kind == Store || a.Kind == Append || a.Kind == Parsers
}

// usageParts returns the usage parts of the actions, optionals first, and the index of the first
// positional.
//
// py: Lib/argparse.py HelpFormatter._get_actions_usage_parts (Python 3.15)
func (f *formatter) usageParts(p *Parser) ([]string, int) {
	t := f.theme
	var optionals, positionals []string
	for _, a := range p.Actions {
		if len(a.Options) == 0 {
			positionals = append(positionals, t.summaryAction+formatArgs(a)+t.reset)
			continue
		}
		option := a.Options[0]
		color := t.summaryShortOption
		if len(option) > 2 {
			color = t.summaryLongOption
		}
		part := color + option + t.reset
		if takesValue(a) {
			part = color + option + " " + t.summaryLabel + formatArgs(a) + t.reset
		}
		if !a.Required {
			part = "[" + part + "]"
		}
		optionals = append(optionals, part)
	}
	return append(optionals, positionals...), len(optionals)
}

// formatUsage returns the usage block, which ends with a blank line.
//
// py: Lib/argparse.py HelpFormatter._format_usage (Python 3.15)
func (f *formatter) formatUsage(p *Parser) string {
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

// subactions returns the choice pseudo-actions of a Parsers action, which list each subcommand.
//
// py: Lib/argparse.py _SubParsersAction._get_subactions (Python 3.15)
func subactions(a *Action) []*Action {
	if a.Kind != Parsers {
		return nil
	}
	return []*Action{{Kind: Store, Dest: a.Choices[0], Help: a.ChoiceHelp}}
}

// py: Lib/argparse.py HelpFormatter._format_action_invocation (Python 3.15)
func (f *formatter) invocation(a *Action) string {
	t := f.theme
	if len(a.Options) == 0 {
		return t.action + metavar(a) + t.reset
	}
	var options []string
	for _, option := range a.Options {
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
func (f *formatter) formatHelp(p *Parser) string {
	t := f.theme
	var positionals, optionals []*Action
	for _, a := range p.Actions {
		if len(a.Options) == 0 {
			positionals = append(positionals, a)
		} else {
			optionals = append(optionals, a)
		}
	}
	sections := []struct {
		heading string
		actions []*Action
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

// formatAction returns the help of one action.
// ponytail: _expand_help's %-formatting is not ported; no help text here holds "%".
//
// py: Lib/argparse.py HelpFormatter._format_action (Python 3.15)
func (f *formatter) formatAction(a *Action, indent int) string {
	helpPosition := min(f.actionMaxLength+2, f.maxHelpPosition)
	helpWidth := max(f.width-helpPosition, 11)
	actionWidth := helpPosition - indent - 2
	header := f.invocation(a)
	plain := decolor(header)
	var out strings.Builder
	indentFirst := 0
	switch {
	case a.Help == "":
		out.WriteString(strings.Repeat(" ", indent) + header + "\n")
	case len(plain) <= actionWidth:
		padded := strings.Repeat(" ", indent) + plain + strings.Repeat(" ", actionWidth-len(plain)) + "  "
		out.WriteString(strings.ReplaceAll(padded, plain, header))
	default:
		out.WriteString(strings.Repeat(" ", indent) + header + "\n")
		indentFirst = helpPosition
	}
	if str.Strip(a.Help) != "" {
		lines := splitLines(a.Help, helpWidth)
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

// asciiWhitespace is HelpFormatter._whitespace_matcher, re.compile(r'\s+', re.ASCII).
var asciiWhitespace = regexp.MustCompile(`[\t\n\v\f\r ]+`)

// py: Lib/argparse.py HelpFormatter._split_lines (Python 3.15)
func splitLines(text string, width int) []string {
	return wrap(str.Strip(asciiWhitespace.ReplaceAllString(text, " ")), width)
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
