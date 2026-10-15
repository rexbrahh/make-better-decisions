// Package argparse ports the subset of CPython 3.15's argparse that the agent-decisions commands use: help,
// store, store_true and append optionals, one subparsers positional, required arguments, abbreviated
// options, and plain usage, help and error text.
//
// A Parser retains ArgumentParser's argument semantics and returns an *Exit where argparse raises SystemExit.
package argparse

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

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
// to those of its parent.
type Parser struct {
	Prog           string
	Actions        []*Action
	Stdout, Stderr io.Writer
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
			sub.Stdout, sub.Stderr = p.Stdout, p.Stderr
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
	return &argumentError{a, fmt.Sprintf("invalid choice: %s (choose from %s)", str.Repr(value), choices)}
}

// Error prints the usage and message to Stderr and returns the SystemExit of status 2.
//
// py: Lib/argparse.py ArgumentParser.error (Python 3.15)
func (p *Parser) Error(message string) *Exit {
	p.printUsage(p.Stderr)
	write(p.Stderr, fmt.Sprintf("%s: error: %s\n", p.Prog, message))
	return &Exit{2}
}

// py: Lib/argparse.py ArgumentParser.print_usage (Python 3.15)
func (p *Parser) printUsage(w io.Writer) {
	f := newFormatter(p.Prog)
	write(w, strings.Trim(f.formatUsage(p), "\n")+"\n")
}

// py: Lib/argparse.py ArgumentParser.print_help (Python 3.15)
func (p *Parser) printHelp(w io.Writer) {
	write(w, newFormatter(p.Prog).formatHelp(p))
}

// write ports _print_message: it writes s with the backslashreplace error handler of sys.stderr and
// ignores write errors.
func write(w io.Writer, s string) {
	io.WriteString(w, pyerr.EncodeBackslashReplace(s))
}

// formatter prints plain help without terminal-dependent layout.
type formatter struct{ prog string }

func newFormatter(prog string) *formatter { return &formatter{prog: prog} }

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

// usageParts lists options before positionals, as the parser's usage does.
func (f *formatter) usageParts(p *Parser) []string {
	var optionals, positionals []string
	for _, a := range p.Actions {
		if len(a.Options) == 0 {
			positionals = append(positionals, formatArgs(a))
			continue
		}
		part := a.Options[0]
		if takesValue(a) {
			part += " " + formatArgs(a)
		}
		if !a.Required {
			part = "[" + part + "]"
		}
		optionals = append(optionals, part)
	}
	return append(optionals, positionals...)
}

func (f *formatter) formatUsage(p *Parser) string {
	return "usage: " + strings.Join(append([]string{f.prog}, f.usageParts(p)...), " ") + "\n\n"
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

// invocation lists an argument's names and value placeholder.
func (f *formatter) invocation(a *Action) string {
	if len(a.Options) == 0 {
		return metavar(a)
	}
	text := strings.Join(a.Options, ", ")
	if takesValue(a) {
		text += " " + formatArgs(a)
	}
	return text
}

func (f *formatter) formatHelp(p *Parser) string {
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
	var out strings.Builder
	out.WriteString(strings.TrimRight(f.formatUsage(p), "\n"))
	for _, section := range sections {
		if len(section.actions) == 0 {
			continue
		}
		out.WriteString("\n\n" + section.heading + ":\n")
		for _, a := range section.actions {
			out.WriteString(f.formatAction(a, 2))
		}
	}
	return strings.TrimRight(out.String(), "\n") + "\n"
}

func (f *formatter) formatAction(a *Action, indent int) string {
	text := strings.Repeat(" ", indent) + f.invocation(a)
	if help := strings.Join(strings.Fields(a.Help), " "); help != "" {
		text += "  " + help
	}
	text += "\n"
	for _, sub := range subactions(a) {
		text += f.formatAction(sub, indent+2)
	}
	return text
}
