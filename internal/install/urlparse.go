package install

// The parts of CPython 3.15's urllib.parse and ipaddress that _redact_url uses: urlsplit with its netloc
// checks, urlunsplit, parse_qsl(keep_blank_values=True) and urlencode(doseq=True).

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// splitResult is a SplitResult whose missing parts are "".
type splitResult struct {
	scheme, netloc, path, query, fragment string
}

func valueError(msg string) error {
	return &pyerr.Error{Class: "ValueError", Msg: msg}
}

// urlsplit ports urllib.parse.urlsplit(url). The leading-control strip and the tab and newline removal of
// _urlsplit never apply here: every URL starts with "http" and holds no whitespace.
//
// py: Lib/urllib/parse.py urlsplit, _urlsplit (Python 3.15)
func urlsplit(url string) (splitResult, error) {
	var parts splitResult
	// scheme_chars and ':' are ASCII, so byte offsets equal code point offsets, and lower() is ASCII.
	if i := strings.IndexByte(url, ':'); i > 0 && isASCIIAlpha(url[0]) &&
		strings.Trim(url[:i], schemeChars) == "" {
		parts.scheme, url = strings.ToLower(url[:i]), url[i+1:]
	}
	if strings.HasPrefix(url, "//") {
		end := len(url)
		if i := strings.IndexAny(url[2:], "/?#"); i >= 0 {
			end = 2 + i
		}
		parts.netloc, url = url[2:end], url[end:]
		open, close := strings.Contains(parts.netloc, "["), strings.Contains(parts.netloc, "]")
		if open != close {
			return parts, valueError("Invalid IPv6 URL")
		}
		if open {
			if err := checkBracketedNetloc(parts.netloc); err != nil {
				return parts, err
			}
		}
	}
	url, parts.fragment, _ = strings.Cut(url, "#")
	parts.path, parts.query, _ = strings.Cut(url, "?")
	if err := checkNetloc(parts.netloc); err != nil {
		return parts, err
	}
	return parts, nil
}

const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

func isASCIIAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// nfkcSeparators are the code points whose NFKC form holds one of "/?#@:". oracle_test.go derives the
// same set from unicodedata.
var nfkcSeparators = []rune{0x2047, 0x2048, 0x2049, 0x2100, 0x2101, 0x2105, 0x2106, 0x2a74, 0xfe13, 0xfe16,
	0xfe55, 0xfe56, 0xfe5f, 0xfe6b, 0xff03, 0xff0f, 0xff1a, 0xff1f, 0xff20}

// checkNetloc rejects a netloc whose NFKC normalization adds a URL separator. The netloc holds no '/',
// '?' or '#', and _checknetloc removes '@' and ':' before it normalizes, so a separator in the normalized
// text can only come from one of nfkcSeparators.
//
// py: Lib/urllib/parse.py _checknetloc (Python 3.15)
func checkNetloc(netloc string) error {
	for rest := netloc; rest != ""; {
		r, size := str.DecodeRune(rest)
		if slices.Contains(nfkcSeparators, r) {
			return valueError("netloc '" + netloc + "' contains invalid characters under NFKC normalization")
		}
		rest = rest[size:]
	}
	return nil
}

// py: Lib/urllib/parse.py _check_bracketed_netloc (Python 3.15)
func checkBracketedNetloc(netloc string) error {
	hostAndPort := netloc[strings.LastIndex(netloc, "@")+1:]
	var hostname string
	if before, bracketed, found := strings.Cut(hostAndPort, "["); found {
		if before != "" {
			return valueError("Invalid IPv6 URL")
		}
		var port string
		hostname, port, _ = strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return valueError("Invalid IPv6 URL")
		}
	} else {
		hostname, _, _ = strings.Cut(hostAndPort, ":")
	}
	return checkBracketedHost(hostname)
}

// ipvFuture is the IPvFuture pattern; Go's . excludes only "\n", as Python's does.
var ipvFuture = regexp.MustCompile(`\A[vV][a-fA-F0-9]+\..+\z`)

// py: Lib/urllib/parse.py _check_bracketed_host (Python 3.15)
func checkBracketedHost(hostname string) error {
	if strings.HasPrefix(hostname, "v") || strings.HasPrefix(hostname, "V") {
		if !ipvFuture.MatchString(hostname) {
			return valueError("IPvFuture address is invalid")
		}
		return nil
	}
	if isIPv4(hostname) {
		return valueError("An IPv4 address cannot be in brackets")
	}
	if !isIPv6(hostname) {
		return valueError(str.Repr(hostname) + " does not appear to be an IPv4 or IPv6 address")
	}
	return nil
}

// isIPv4 reports whether ipaddress.IPv4Address(text) accepts text.
//
// py: Lib/ipaddress.py IPv4Address.__init__, _ip_int_from_string, _parse_octet (Python 3.15)
func isIPv4(text string) bool {
	if text == "" || strings.Contains(text, "/") {
		return false
	}
	octets := strings.Split(text, ".")
	if len(octets) != 4 {
		return false
	}
	for _, octet := range octets {
		if octet == "" || len(octet) > 3 || strings.Trim(octet, "0123456789") != "" ||
			octet != "0" && octet[0] == '0' {
			return false
		}
		if n, _ := strconv.Atoi(octet); n > 255 {
			return false
		}
	}
	return true
}

// isIPv6 reports whether ipaddress.IPv6Address(text) accepts text.
//
// py: Lib/ipaddress.py IPv6Address.__init__, _split_scope_id, _ip_int_from_string (Python 3.15)
func isIPv6(text string) bool {
	if strings.Contains(text, "/") {
		return false
	}
	address, scope, found := strings.Cut(text, "%")
	if found && (scope == "" || strings.Contains(scope, "%")) {
		return false
	}
	if address == "" || str.Len(address) > 45 {
		return false
	}
	const hextets = 8
	parts := strings.SplitN(address, ":", hextets+2) // split(':', maxsplit=9)
	if len(parts) < 3 {
		return false
	}
	if last := parts[len(parts)-1]; strings.Contains(last, ".") {
		if !isIPv4(last) {
			return false
		}
		parts = append(parts[:len(parts)-1], "0", "0") // two valid hextets
	}
	if len(parts) > hextets+1 {
		return false
	}
	skip := -1
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			if skip >= 0 {
				return false
			}
			skip = i
		}
	}
	var hi, lo int
	if skip >= 0 {
		hi, lo = skip, len(parts)-skip-1
		if parts[0] == "" {
			if hi--; hi != 0 {
				return false
			}
		}
		if parts[len(parts)-1] == "" {
			if lo--; lo != 0 {
				return false
			}
		}
		if hextets-(hi+lo) < 1 {
			return false
		}
	} else {
		if len(parts) != hextets || parts[0] == "" || parts[len(parts)-1] == "" {
			return false
		}
		hi = len(parts)
	}
	for _, part := range slices.Concat(parts[:hi], parts[len(parts)-lo:]) {
		if part == "" || len(part) > 4 || strings.Trim(part, "0123456789abcdefABCDEF") != "" {
			return false
		}
	}
	return true
}

// usesNetloc is urllib.parse.uses_netloc.
var usesNetloc = []string{"", "ftp", "http", "gopher", "nntp", "telnet", "imap", "wais", "file", "mms", "https",
	"shttp", "snews", "prospero", "rtsp", "rtsps", "rtspu", "rsync", "svn", "svn+ssh", "sftp", "nfs", "git",
	"git+ssh", "ws", "wss", "itms-services"}

// urlunsplit ports urlunsplit(parts) for a plain tuple: empty parts count as missing.
//
// py: Lib/urllib/parse.py urlunsplit, _urlunsplit (Python 3.15)
func urlunsplit(parts splitResult) string {
	url := parts.path
	hasNetloc := parts.netloc != "" ||
		parts.scheme != "" && slices.Contains(usesNetloc, parts.scheme) && (url == "" || url[0] == '/')
	if hasNetloc {
		if url != "" && url[0] != '/' {
			url = "/" + url
		}
		url = "//" + parts.netloc + url
	} else if strings.HasPrefix(url, "//") {
		url = "//" + url
	}
	if parts.scheme != "" {
		url = parts.scheme + ":" + url
	}
	if parts.query != "" {
		url += "?" + parts.query
	}
	if parts.fragment != "" {
		url += "#" + parts.fragment
	}
	return url
}

// parseQSL returns parse_qsl(query, keep_blank_values=True).
//
// py: Lib/urllib/parse.py parse_qsl (Python 3.15)
func parseQSL(query string) [][2]string {
	var pairs [][2]string
	for _, field := range strings.Split(query, "&") {
		if field == "" {
			continue
		}
		name, value, _ := strings.Cut(field, "=")
		pairs = append(pairs, [2]string{unquotePlus(name), unquotePlus(value)})
	}
	return pairs
}

// py: Lib/urllib/parse.py unquote_plus (Python 3.15)
func unquotePlus(s string) string {
	return unquote(strings.ReplaceAll(s, "+", " "))
}

// unquote decodes %XX escapes in each run of ASCII as UTF-8 with the replace error handler, and keeps
// the other code points as they are.
//
// py: Lib/urllib/parse.py unquote, _generate_unquoted_parts, _unquote_impl (Python 3.15)
func unquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	for s != "" {
		end := 0
		for end < len(s) && s[end] < 0x80 {
			end++
		}
		if end == 0 { // a non-ASCII run
			for end < len(s) && s[end] >= 0x80 {
				end++
			}
			out.WriteString(s[:end])
			s = s[end:]
			continue
		}
		bits := strings.Split(s[:end], "%")
		decoded := []byte(bits[0])
		for _, bit := range bits[1:] {
			if len(bit) >= 2 && isHex(bit[0]) && isHex(bit[1]) {
				b, _ := strconv.ParseUint(bit[:2], 16, 8)
				decoded = append(append(decoded, byte(b)), bit[2:]...)
			} else {
				decoded = append(append(decoded, '%'), bit...)
			}
		}
		out.WriteString(json.DecodeUTF8(decoded))
		s = s[end:]
	}
	return out.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// urlencode returns urlencode(pairs, doseq=True) for str pairs.
//
// py: Lib/urllib/parse.py urlencode (Python 3.15)
func urlencode(pairs [][2]string) (string, error) {
	fields := make([]string, len(pairs))
	for i, pair := range pairs {
		key, err := quotePlus(pair[0])
		if err != nil {
			return "", err
		}
		value, err := quotePlus(pair[1])
		if err != nil {
			return "", err
		}
		fields[i] = key + "=" + value
	}
	return strings.Join(fields, "&"), nil
}

// quotePlus returns quote_plus(s): strict UTF-8, with unreserved bytes kept, ' ' as '+' and the rest as
// %XX.
//
// py: Lib/urllib/parse.py quote_plus, quote, quote_from_bytes (Python 3.15)
func quotePlus(s string) (string, error) {
	data, err := pyerr.EncodeUTF8(s)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, b := range []byte(data) {
		switch {
		case isASCIIAlpha(b) || b >= '0' && b <= '9' || strings.IndexByte("_.-~", b) >= 0:
			out.WriteByte(b)
		case b == ' ':
			out.WriteByte('+')
		default:
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}
	return out.String(), nil
}
