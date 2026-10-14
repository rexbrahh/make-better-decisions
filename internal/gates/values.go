package gates

import (
	"fmt"
	"math"
	"math/big"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/ospath"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// This file holds the Python semantics that gates.py gets from the language: subscripts, .get, comparisons
// and formatting on decoded JSON values, str() and repr(), the recursion limit, and the pathlib calls.

// recursionLimit is sys.getrecursionlimit(): Python raises RecursionError on entering a deeper frame.
const recursionLimit = 1000

func recursionError() error {
	return &json.RecursionError{Msg: "maximum recursion depth exceeded"}
}

// expr evaluates Python subscripts, .get calls, comparisons and formatting on decoded JSON values. It keeps
// the first exception, and every later step returns a zero value without effect, so a Go expression written
// in the Python evaluation order reports the exception that Python raises first. Check err before any side
// effect.
type expr struct{ err error }

func (e *expr) fail(class, format string, args ...any) {
	if e.err == nil {
		e.err = &pyerr.Error{Class: class, Msg: fmt.Sprintf(format, args...)}
	}
}

// item returns v[key] for a str key.
func (e *expr) item(v any, key string) any {
	if e.err != nil {
		return nil
	}
	switch v := v.(type) {
	case *json.Object:
		if value, ok := v.Get(key); ok {
			return value
		}
		e.fail("KeyError", "%s", pystr.Repr(key))
	case []any:
		e.fail("TypeError", "list indices must be integers or slices, not str")
	case string:
		e.fail("TypeError", "string indices must be integers, not 'str'")
	default:
		e.fail("TypeError", "'%s' object is not subscriptable", json.TypeName(v))
	}
	return nil
}

// get returns v.get(key, def). key may be any decoded value: Python hashes it first.
func (e *expr) get(v, key, def any) any {
	if e.err != nil {
		return nil
	}
	object, ok := v.(*json.Object)
	if !ok {
		e.fail("AttributeError", "'%s' object has no attribute 'get'", json.TypeName(v))
		return nil
	}
	name, found := e.key(key, "a dict key")
	if !found {
		return def
	}
	if value, ok := object.Get(name); ok {
		return value
	}
	return def
}

// key returns key as a dict key or set element of str keys. found is false for a hashable key that is not
// a str, which no such key equals; an unhashable key is the TypeError that hashing raises.
func (e *expr) key(key any, as string) (name string, found bool) {
	if e.err != nil {
		return "", false
	}
	switch key := key.(type) {
	case string:
		return key, true
	case []any, *json.Object:
		name := json.TypeName(key)
		e.fail("TypeError", "cannot use '%s' as %s (unhashable type: '%s')", name, as, name)
	}
	return "", false
}

// in returns `v in set` for a set of str elements.
func (e *expr) in(v any, set map[string]bool) bool {
	name, found := e.key(v, "a set element")
	return found && set[name]
}

// iter returns the items that `for x in v` visits: a dict's keys, a str's characters.
func (e *expr) iter(v any) []any {
	if e.err != nil {
		return nil
	}
	switch v := v.(type) {
	case []any:
		return v
	case *json.Object:
		keys := make([]any, 0, v.Len())
		for _, key := range v.Keys() {
			keys = append(keys, key)
		}
		return keys
	case string:
		var chars []any
		for offset := 0; offset < len(v); {
			_, size := pystr.DecodeRune(v[offset:])
			chars = append(chars, v[offset:offset+size])
			offset += size
		}
		return chars
	}
	e.fail("TypeError", "'%s' object is not iterable", json.TypeName(v))
	return nil
}

// items returns the keys of v for v.items(); the values are v.Value(key).
func (e *expr) items(v any) *json.Object {
	if e.err != nil {
		return &json.Object{}
	}
	object, ok := v.(*json.Object)
	if !ok {
		e.fail("AttributeError", "'%s' object has no attribute 'items'", json.TypeName(v))
		return &json.Object{}
	}
	return object
}

// number returns v as an exact value for a Python int, bool or float. nan reports a float NaN.
func number(v any) (value *big.Float, nan, ok bool) {
	switch v := v.(type) {
	case bool:
		if v {
			return big.NewFloat(1), false, true
		}
		return big.NewFloat(0), false, true
	case float64:
		if math.IsNaN(v) {
			return nil, true, true
		}
		return big.NewFloat(v), false, true
	case json.Int:
		n, _ := new(big.Int).SetString(string(v), 10)
		return new(big.Float).SetInt(n), false, true
	}
	return nil, false, false
}

// compare returns `a op b` for numbers, which Python compares exactly, or the TypeError for other types.
func (e *expr) compare(a any, op string, b any) bool {
	if e.err != nil {
		return false
	}
	x, xNaN, xOK := number(a)
	y, yNaN, yOK := number(b)
	if !xOK || !yOK {
		e.fail("TypeError", "'%s' not supported between instances of '%s' and '%s'", op, json.TypeName(a), json.TypeName(b))
		return false
	}
	if xNaN || yNaN {
		return false
	}
	c := x.Cmp(y)
	switch op {
	case ">=":
		return c >= 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	}
	panic("gates: unknown comparison " + op)
}

func (e *expr) ge(v any, threshold float64) bool { return e.compare(v, ">=", threshold) }
func (e *expr) lt(v any, threshold float64) bool { return e.compare(v, "<", threshold) }
func (e *expr) le(v any, threshold float64) bool { return e.compare(v, "<=", threshold) }
func (e *expr) gt(v any, threshold float64) bool { return e.compare(v, ">", threshold) }

// max returns max(a, b): b only when b > a.
func (e *expr) max(a, b any) any {
	if e.compare(b, ">", a) {
		return b
	}
	return a
}

// negated returns -v as a sort key. Jev answers hold probabilities, so a float key is exact.
func (e *expr) negated(v any) float64 {
	if e.err != nil {
		return 0
	}
	switch v := v.(type) {
	case bool:
		if v {
			return -1
		}
		return 0
	case float64:
		return -v
	case json.Int:
		f, _ := strconv.ParseFloat(string(v), 64)
		return -f
	}
	e.fail("TypeError", "bad operand type for unary -: '%s'", json.TypeName(v))
	return 0
}

// fixed2 returns f"{v:.2f}".
func (e *expr) fixed2(v any) string {
	if e.err != nil {
		return ""
	}
	switch v := v.(type) {
	case bool:
		if v {
			return "1.00"
		}
		return "0.00"
	case float64:
		return formatFixed2(v)
	case json.Int:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			e.fail("OverflowError", "int too large to convert to float")
			return ""
		}
		return formatFixed2(f)
	case string:
		e.fail("ValueError", "Unknown format code 'f' for object of type 'str'")
	default:
		e.fail("TypeError", "unsupported format string passed to %s.__format__", json.TypeName(v))
	}
	return ""
}

// formatFixed2 returns format(f, ".2f"). Both Python and strconv round the exact binary value.
func formatFixed2(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// equal returns a == b, keeping the RecursionError of a too deep comparison.
func (e *expr) equal(a, b any) bool {
	if e.err != nil {
		return false
	}
	same, err := json.Equal(a, b)
	if err != nil {
		e.err = err
	}
	return same
}

// display returns str(v) for a decoded JSON value.
func display(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	return json.Repr(v)
}

// isValueError reports whether err is a Python ValueError or one of its subclasses.
func isValueError(err error) bool {
	typed, ok := err.(interface{ PyType() string })
	if !ok {
		return false
	}
	switch typed.PyType() {
	case "ValueError", "RequestError", "JSONDecodeError", "UnicodeDecodeError", "UnicodeEncodeError":
		return true
	}
	return false
}

// getcwd ports os.getcwd(), which reads the kernel's path rather than $PWD.
func getcwd() (string, error) {
	cwd, err := syscall.Getwd()
	if err != nil {
		return "", pyerr.NewOSError(err)
	}
	return cwd, nil
}

// pathOf returns str(Path(cwd or os.getcwd())) for a decoded cwd value.
//
// py: Lib/pathlib/_local.py PurePath.__init__ (Python 3.15)
func pathOf(cwd any) (string, error) {
	if !json.Truthy(cwd) {
		dir, err := getcwd()
		return pathlib.Str(dir), err
	}
	dir, ok := cwd.(string)
	if !ok {
		return "", &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf(
			"argument should be a str or an os.PathLike object where __fspath__ returns a str, not '%s'", json.TypeName(cwd))}
	}
	return pathlib.Str(dir), nil
}

// home returns str(Path.home()).
//
// py: Lib/pathlib/_local.py Path.home, Path.expanduser (Python 3.15)
func home() (string, error) {
	dir, err := ospath.ExpandUser("~")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(dir, "~") {
		return "", &pyerr.Error{Class: "RuntimeError", Msg: "Could not determine home directory."}
	}
	return pathlib.Str(dir), nil
}

// resolve returns str(Path(p).resolve()).
//
// py: Lib/pathlib/_local.py Path.resolve (Python 3.15)
func resolve(p string) (string, error) {
	return ospath.Realpath(pathlib.Str(p), false)
}

// isAbs returns Path(p).is_absolute() on POSIX.
func isAbs(p string) bool {
	return strings.HasPrefix(p, "/")
}

// parts returns Path(p).parts on POSIX: the anchor, then each name. Exactly two leading slashes are their
// own anchor.
//
// py: Lib/pathlib/_local.py PurePath.parts (Python 3.15)
func parts(p string) []string {
	var result []string
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		result = append(result, "//")
	case strings.HasPrefix(p, "/"):
		result = append(result, "/")
	}
	for _, name := range strings.Split(p, "/") {
		if name != "" && name != "." {
			result = append(result, name)
		}
	}
	return result
}

// relativeTo returns str(path.relative_to(base)) for resolved absolute paths, or false where Python raises
// ValueError.
//
// py: Lib/pathlib/_local.py PurePath.relative_to (Python 3.15)
func relativeTo(path, base string) (string, bool) {
	switch {
	case path == base:
		return ".", true
	case base == "/" && strings.HasPrefix(path, "/"):
		return path[1:], true
	case strings.HasPrefix(path, base+"/"):
		return path[len(base)+1:], true
	}
	return "", false
}

// basename returns os.path.basename(p).
func basename(p string) string {
	return p[strings.LastIndexByte(p, '/')+1:]
}

// darwin is sys.platform == "darwin".
const darwin = runtime.GOOS == "darwin"
