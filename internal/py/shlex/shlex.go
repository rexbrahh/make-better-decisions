// Package shlex ports CPython's Lib/shlex.py, so a command splits into the same tokens that the Python
// implementation produces. A character is one code point; a byte that is not valid UTF-8 counts as one character.
package shlex

import (
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// DefaultPunctuation is the set that punctuation_chars=True selects.
//
// py: Lib/shlex.py shlex.__init__ (Python 3.15)
const DefaultPunctuation = "();<>|&"

const (
	asciiWordchars  = "abcdfeghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"
	latin1Wordchars = "ßàáâãäåæçèéêëìíîïðñòóôõöøùúûüýþÿÀÁÂÃÄÅÆÇÈÉÊËÌÍÎÏÐÑÒÓÔÕÖØÙÚÛÜÝÞ"
	// stateEOF is Python's state None. Every other state is " ", "a", "c" or a non-empty quote or escape character.
	stateEOF = ""
)

// Error is the ValueError that shlex raises. Error() is the Python message.
//
// py: Lib/shlex.py shlex.read_token (Python 3.15)
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

// Stream is the instream that a Lexer reads. Read is instream.read(1): one character, or "" at the end.
// ReadLine is instream.readline(); the Lexer calls it only to skip a comment.
//
// py: Lib/shlex.py shlex.read_token (Python 3.15)
type Stream interface {
	Read() string
	ReadLine() string
}

// StringStream is the io.StringIO that shlex wraps around a string. Positions count characters, as StringIO does.
//
// py: Lib/shlex.py shlex.__init__ (Python 3.15)
type StringStream struct {
	text     string
	offset   int   // byte offset of the next character
	position int64 // character index of the next character; it can pass the end after a Seek
}

// NewStringStream returns a stream positioned at the start of s.
//
// py: Lib/shlex.py shlex.__init__ (Python 3.15)
func NewStringStream(s string) *StringStream {
	return &StringStream{text: s}
}

// Read returns the next character, or "" at the end. It is StringIO.read(1).
//
// py: Lib/shlex.py shlex.read_token (Python 3.15)
func (s *StringStream) Read() string {
	if s.offset >= len(s.text) {
		return ""
	}
	char := firstChar(s.text[s.offset:])
	s.offset += len(char)
	s.position++
	return char
}

// ReadLine returns the characters up to and including the next "\n", or the rest of the text. It is StringIO.readline().
//
// py: Lib/shlex.py shlex.read_token (Python 3.15)
func (s *StringStream) ReadLine() string {
	rest := s.text[s.offset:]
	line := rest
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		line = rest[:i+1]
	}
	s.offset += len(line)
	s.position += int64(utf8.RuneCountInString(line))
	return line
}

// Tell returns the character index of the next read. It is StringIO.tell().
//
// py: Lib/shlex.py shlex.read_token (Python 3.15)
func (s *StringStream) Tell() int64 {
	return s.position
}

// Seek moves to a character index, as StringIO.seek(offset) does. Only io.SeekStart is supported. An index past
// the end is kept, and reads there return "".
//
// py: Lib/shlex.py shlex.read_token (Python 3.15)
func (s *StringStream) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return s.position, errors.New("shlex.StringStream supports only io.SeekStart")
	}
	if offset < 0 {
		return s.position, errors.New("negative seek position")
	}
	s.offset = 0
	for skipped := int64(0); skipped < offset && s.offset < len(s.text); skipped++ {
		s.offset += len(firstChar(s.text[s.offset:]))
	}
	s.position = offset
	return offset, nil
}

// Lexer is a shlex.shlex instance. The exported fields are the Python attributes of the same names; a change
// after New takes effect on the next read, as in Python. The posix and punctuation_chars choices are fixed by New:
// Python derives eof and the wordchars from them in the constructor.
//
// py: Lib/shlex.py shlex (Python 3.15)
type Lexer struct {
	Commenters      string
	Wordchars       string
	Whitespace      string
	WhitespaceSplit bool
	Quotes          string
	Escape          string
	EscapedQuotes   string
	Lineno          int

	stream           Stream
	posix            bool
	punctuationChars string
	state            string
	token            string
	pushback         []string // push_token stack; the last element is the left end of Python's deque
	pushbackChars    []string // _pushback_chars stack; the last element is the right end of Python's deque
}

// New returns a lexer over s. It is shlex.shlex(s, posix=posix, punctuation_chars=punctuationChars), where
// punctuationChars "" is False and DefaultPunctuation is True.
//
// py: Lib/shlex.py shlex.__init__ (Python 3.15)
func New(s string, posix bool, punctuationChars string) *Lexer {
	return NewStream(NewStringStream(s), posix, punctuationChars)
}

// NewStream returns a lexer over input, with the same arguments as New.
//
// py: Lib/shlex.py shlex.__init__ (Python 3.15)
func NewStream(input Stream, posix bool, punctuationChars string) *Lexer {
	l := &Lexer{
		Commenters:       "#",
		Wordchars:        asciiWordchars,
		Whitespace:       " \t\r\n",
		Quotes:           `'"`,
		Escape:           `\`,
		EscapedQuotes:    `"`,
		Lineno:           1,
		stream:           input,
		posix:            posix,
		punctuationChars: punctuationChars,
		state:            " ",
	}
	if posix {
		l.Wordchars += latin1Wordchars
	}
	if punctuationChars != "" {
		// These characters are allowed in file names, arguments and wildcards.
		l.Wordchars += "~-./*?="
		l.Wordchars = strings.Map(func(r rune) rune {
			if contains(punctuationChars, string(r)) {
				return -1
			}
			return r
		}, l.Wordchars)
	}
	return l
}

// PunctuationChars returns the punctuation set fixed by New. Python exposes it as a read-only property.
//
// py: Lib/shlex.py shlex.punctuation_chars (Python 3.15)
func (l *Lexer) PunctuationChars() string {
	return l.punctuationChars
}

// PushToken pushes a token that the next GetToken returns first.
//
// py: Lib/shlex.py shlex.push_token (Python 3.15)
func (l *Lexer) PushToken(token string) {
	l.pushback = append(l.pushback, token)
}

// GetToken returns the next token. ok is false at the end of input, where Python returns self.eof (None in posix
// mode, "" otherwise). An unbalanced quote or a trailing escape returns an *Error.
//
// py: Lib/shlex.py shlex.get_token (Python 3.15)
func (l *Lexer) GetToken() (token string, ok bool, err error) {
	if n := len(l.pushback); n > 0 {
		token = l.pushback[n-1]
		l.pushback = l.pushback[:n-1]
		return token, l.posix || token != "", nil
	}
	token, isNone, err := l.readToken()
	if err != nil {
		return "", false, err
	}
	if l.posix {
		return token, !isNone, nil
	}
	return token, token != "", nil
}

// Tokens returns every remaining token. It is list(lexer), so an error discards the tokens read before it.
//
// py: Lib/shlex.py shlex.__next__ (Python 3.15)
func (l *Lexer) Tokens() ([]string, error) {
	tokens := []string{}
	for {
		token, ok, err := l.GetToken()
		if err != nil {
			return nil, err
		}
		if !ok {
			return tokens, nil
		}
		tokens = append(tokens, token)
	}
}

// readToken returns the next raw token. isNone reports Python's None, which only posix mode returns.
//
// py: Lib/shlex.py shlex.read_token (Python 3.15)
func (l *Lexer) readToken() (token string, isNone bool, err error) {
	quoted := false
	escapedState := " "
	// emit is Python's `self.token or (self.posix and quoted)`.
	emit := func() bool { return l.token != "" || l.posix && quoted }
scan:
	for {
		var next string
		if n := len(l.pushbackChars); l.punctuationChars != "" && n > 0 {
			next = l.pushbackChars[n-1]
			l.pushbackChars = l.pushbackChars[:n-1]
		} else {
			next = l.stream.Read()
		}
		if next == "\n" {
			l.Lineno++
		}
		switch {
		case l.state == stateEOF:
			l.token = "" // past end of file
			break scan
		case l.state == " ":
			switch {
			case next == "":
				l.state = stateEOF
				break scan
			case contains(l.Whitespace, next):
				if emit() {
					break scan
				}
			case contains(l.Commenters, next):
				l.stream.ReadLine()
				l.Lineno++
			case l.posix && contains(l.Escape, next):
				escapedState = "a"
				l.state = next
			case contains(l.Wordchars, next):
				l.token = next
				l.state = "a"
			case contains(l.punctuationChars, next):
				l.token = next
				l.state = "c"
			case contains(l.Quotes, next):
				if !l.posix {
					l.token = next
				}
				l.state = next
			case l.WhitespaceSplit:
				l.token = next
				l.state = "a"
			default:
				l.token = next // a single non-word character is a token of its own
				break scan
			}
		case contains(l.Quotes, l.state):
			quoted = true
			switch {
			case next == "":
				return "", false, &Error{"No closing quotation"}
			case next == l.state:
				if !l.posix {
					l.token += next
					l.state = " "
					break scan
				}
				l.state = "a"
			case l.posix && contains(l.Escape, next) && contains(l.EscapedQuotes, l.state):
				escapedState = l.state
				l.state = next
			default:
				l.token += next
			}
		case contains(l.Escape, l.state):
			if next == "" {
				return "", false, &Error{"No escaped character"}
			}
			// In posix shells, only the quote itself or the escape character may be escaped within quotes.
			if contains(l.Quotes, escapedState) && next != l.state && next != escapedState {
				l.token += l.state
			}
			l.token += next
			l.state = escapedState
		case l.state == "a" || l.state == "c":
			switch {
			case next == "":
				l.state = stateEOF
				break scan
			case contains(l.Whitespace, next):
				l.state = " "
				if emit() {
					break scan
				}
			case contains(l.Commenters, next):
				l.stream.ReadLine()
				l.Lineno++
				if l.posix {
					l.state = " "
					if emit() {
						break scan
					}
				}
			case l.state == "c":
				if contains(l.punctuationChars, next) {
					l.token += next
					break
				}
				l.pushbackChars = append(l.pushbackChars, next) // whitespace was handled above
				l.state = " "
				break scan
			case l.posix && contains(l.Quotes, next):
				l.state = next
			case l.posix && contains(l.Escape, next):
				escapedState = "a"
				l.state = next
			case contains(l.Wordchars, next) || contains(l.Quotes, next) ||
				l.WhitespaceSplit && !contains(l.punctuationChars, next):
				l.token += next
			default:
				if l.punctuationChars != "" {
					l.pushbackChars = append(l.pushbackChars, next)
				} else {
					l.pushback = append(l.pushback, next)
				}
				l.state = " "
				if emit() {
					break scan
				}
			}
		}
	}
	token = l.token
	l.token = ""
	return token, l.posix && !quoted && token == "", nil
}

// Split splits s like a POSIX shell, without comments. It is shlex.split(s).
//
// py: Lib/shlex.py split (Python 3.15)
func Split(s string) ([]string, error) {
	lexer := New(s, true, "")
	lexer.WhitespaceSplit = true
	lexer.Commenters = ""
	return lexer.Tokens()
}

// Join quotes each argument and joins them with spaces. It is shlex.join(args).
//
// py: Lib/shlex.py join (Python 3.15)
func Join(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = Quote(arg)
	}
	return strings.Join(quoted, " ")
}

// Quote returns s quoted for a POSIX shell. It is shlex.quote(s).
//
// py: Lib/shlex.py quote (Python 3.15)
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, isUnsafe) < 0 {
		return s
	}
	// Use single quotes, and put single quotes into double quotes: $'b becomes '$'"'"'b'.
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// isUnsafe reports whether r is outside quote's ASCII safe set %+,-./0-9:=@A-Z_a-z.
func isUnsafe(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return false
	}
	return !strings.ContainsRune("%+,-./:=@_", r)
}

// firstChar returns the first character of a non-empty s: one code point, or one byte that is not valid UTF-8.
func firstChar(s string) string {
	_, size := utf8.DecodeRuneInString(s)
	return s[:size]
}

// contains is Python's `char in chars` for a single character: "" is in every string, as at end of input.
func contains(chars, char string) bool {
	if char == "" {
		return true
	}
	for chars != "" {
		first := firstChar(chars)
		if first == char {
			return true
		}
		chars = chars[len(first):]
	}
	return false
}
