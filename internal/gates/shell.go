package gates

import (
	"sort"
	"strings"

	"github.com/rexbrahh/make-better-decisions/internal/py/shlex"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

type shellSpan struct{ start, end int }

type shellEdit struct {
	shellSpan
	text string
}

type shellHeredoc struct {
	op, bodyStart, bodyEnd, closeEnd int
	stripTabs                        bool
}

type shellAnalysis struct {
	remaining int64
	exhausted bool
	called    bool
	data      []shellEdit
	ansi      map[shellSpan]string
}

func shellInputFits(size uint64) bool {
	// IsValid alone cannot detect an offset clamped by the parser.
	return size <= uint64(syntax.NewPos(^uint(0), 1, 1).Offset())
}

func shellCommandViews(command string) (source, detection string, called bool) {
	if !shellInputFits(uint64(len(command))) {
		return command, command, false
	}
	// Two dialects, prefix and full passes, plus the same passes for shell stdin.
	a := shellAnalysis{remaining: int64(len(command)) * 8, ansi: make(map[shellSpan]string)}
	a.scan(command, 0)
	if a.exhausted {
		a.data = nil
	}
	source = applyShellEdits(command, a.data)
	edits := append([]shellEdit(nil), a.data...)
	for span, text := range a.ansi {
		edits = append(edits, shellEdit{span, text})
	}
	return source, applyShellEdits(command, edits), a.called
}

func (a *shellAnalysis) scan(source string, base int) {
	if source == "" || a.called {
		return
	}
	doc, hasDoc := firstQuotedShellHeredoc(source)
	type proof struct {
		count    int
		matched  bool
		head     string
		complete bool
	}
	langs := []syntax.LangVariant{syntax.LangBash, syntax.LangZsh}
	proofs := make([]proof, len(langs))
	for i, lang := range langs {
		p := &proofs[i]
		p.complete = a.parse(source, base, lang, func(stmt *syntax.Stmt) {
			if hasDoc && int(stmt.Pos().Offset()) < doc.closeEnd {
				p.count++
				if head, ok := quotedShellHead(stmt, doc); ok {
					p.matched, p.head = true, head
				}
			}
		})
	}
	if !hasDoc {
		return
	}
	exclude, active, successful := true, false, 0
	agreedHead := ""
	for i, lang := range langs {
		p := proofs[i]
		// A completed header survives a later parse error in the suffix.
		if p.count != 1 || !p.matched {
			p = proof{}
			p.complete = a.parse(source[:doc.closeEnd], base, lang, func(stmt *syntax.Stmt) {
				p.count++
				if head, ok := quotedShellHead(stmt, doc); ok {
					p.matched, p.head = true, head
				}
			})
		}
		if !p.complete && (p.count != 1 || !p.matched) {
			continue
		}
		successful++
		active = active || p.matched
		if p.count != 1 || !p.matched || p.head == "" || p.head == "ts-decide" || stdinShells[p.head] {
			exclude = false
		}
		if successful == 1 {
			agreedHead = p.head
		} else if p.head != agreedHead {
			exclude = false
		}
	}
	if exclude && successful > 0 && !a.exhausted {
		a.data = append(a.data, shellEdit{shellSpan{base + doc.bodyStart, base + doc.bodyEnd}, ""})
	} else if active {
		// An outer AST stores even shell stdin as a word, not as shell statements.
		a.scan(source[doc.bodyStart:doc.bodyEnd], base+doc.bodyStart)
	}
}

func (a *shellAnalysis) parse(source string, base int, lang syntax.LangVariant, inspect func(*syntax.Stmt)) bool {
	if int64(len(source)) > a.remaining {
		a.exhausted = true
		return false // Keep raw source when the shared parse budget is exhausted.
	}
	a.remaining -= int64(len(source))
	parser := syntax.NewParser(syntax.Variant(lang))
	for stmt, err := range parser.StmtsSeq(strings.NewReader(source)) {
		if err != nil {
			return false
		}
		if inspect != nil {
			inspect(stmt)
		}
		syntax.Walk(stmt, func(node syntax.Node) bool {
			if a.called {
				return false
			}
			switch node := node.(type) {
			case *syntax.CallExpr:
				if staticShellHead(node) == "ts-decide" {
					a.called = true
					return false
				}
			case *syntax.SglQuoted:
				if !node.Dollar || !node.Pos().IsValid() || !node.End().IsValid() {
					break
				}
				start, end := int(node.Pos().Offset()), int(node.End().Offset())
				if start < 0 || end > len(source) || end-start < 3 || !strings.HasPrefix(source[start:end], "$'") {
					break
				}
				if text, ok := staticShellWord(&syntax.Word{Parts: []syntax.WordPart{node}}); ok {
					// Edits retain original byte spans; no AST is parsed from the replacement view.
					a.ansi[shellSpan{base + start, base + end}] = shlex.Quote(text)
				}
			}
			return true
		})
	}
	return true
}

func quotedShellHead(stmt *syntax.Stmt, doc shellHeredoc) (string, bool) {
	for {
		if timed, ok := stmt.Cmd.(*syntax.TimeClause); ok && timed.Stmt != nil {
			stmt = timed.Stmt
			continue
		}
		break
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(stmt.Redirs) != 1 || stmt.Background || stmt.Coprocess || stmt.Semicolon.IsValid() {
		return "", false
	}
	redirect := stmt.Redirs[0]
	op := syntax.Hdoc
	if doc.stripTabs {
		op = syntax.DashHdoc
	}
	if redirect.Op != op || !redirect.OpPos.IsValid() || int(redirect.OpPos.Offset()) != doc.op {
		return "", false
	}
	return staticShellHead(call), true
}

func staticShellHead(call *syntax.CallExpr) string {
	var argv []string
	for _, word := range call.Args {
		value, ok := staticShellWord(word)
		if !ok {
			break
		}
		argv = append(argv, value)
	}
	return executionHead(argv, 0)
}

func staticShellWord(word *syntax.Word) (string, bool) {
	if len(word.Parts) > 0 {
		if first, ok := word.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(first.Value, "~") {
			return "", false // Literal can otherwise perform a user lookup.
		}
	}
	if !staticShellParts(word.Parts, false) {
		return "", false
	}
	copy := *word
	if syntax.SplitBraces(&copy) {
		return "", false
	}
	value, err := expand.Literal(&expand.Config{}, word)
	return value, err == nil
}

func staticShellParts(parts []syntax.WordPart, quoted bool) bool {
	for _, part := range parts {
		switch part := part.(type) {
		case *syntax.Lit:
			if !quoted && pattern.HasMeta(part.Value, 0) {
				return false
			}
		case *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if part.Dollar || !staticShellParts(part.Parts, true) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func firstQuotedShellHeredoc(source string) (shellHeredoc, bool) {
	for at := 0; at < len(source); {
		end := strings.IndexByte(source[at:], '\n')
		if end < 0 {
			return shellHeredoc{}, false
		}
		end += at
		line := strings.TrimSuffix(source[at:end], "\r")
		if pystr.Strip(line) == "" || strings.HasPrefix(pystr.LStrip(line), "#") {
			at = end + 1
			continue
		}
		header, ok := matchDataHeredoc(line)
		if !ok {
			return shellHeredoc{}, false
		}
		for closeAt := end + 1; closeAt < len(source); {
			closeEnd := strings.IndexByte(source[closeAt:], '\n')
			if closeEnd < 0 {
				closeEnd = len(source)
			} else {
				closeEnd += closeAt
			}
			closing := strings.TrimSuffix(source[closeAt:closeEnd], "\r")
			if header.stripTabs {
				closing = strings.TrimLeft(closing, "\t")
			}
			if closing == header.delimiter {
				return shellHeredoc{at + len(header.command), end + 1, closeAt, min(closeEnd+1, len(source)), header.stripTabs}, true
			}
			closeAt = closeEnd + 1
		}
		return shellHeredoc{}, false
	}
	return shellHeredoc{}, false
}

func applyShellEdits(source string, edits []shellEdit) string {
	if len(edits) == 0 {
		return source
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end > edits[j].end
		}
		return edits[i].start < edits[j].start
	})
	var out strings.Builder
	at := 0
	for _, edit := range edits {
		if edit.start < 0 || edit.end > len(source) || edit.start > edit.end {
			return source
		}
		if edit.start < at {
			if edit.end <= at {
				continue
			}
			return source
		}
		out.WriteString(source[at:edit.start])
		out.WriteString(edit.text)
		at = edit.end
	}
	out.WriteString(source[at:])
	return out.String()
}
