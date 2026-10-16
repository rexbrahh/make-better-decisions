// Package jev is the TypeSafe System One client. Every failure becomes a *JevError with a short reason.
//
// py: agent_decisions/jev.py
package jev

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rexbrahh/make-better-decisions/internal/evidence"
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	pystr "github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Side effects that tests replace. Python reads policy.KEY_FILE and policy.URL at call time, so a test
// patches them there; here they are package variables.
var (
	now                         = time.Now
	keyFile                     = policy.KeyFile
	url                         = policy.URL
	transport http.RoundTripper = http.DefaultTransport
)

// JevError is a failed Jev exchange. Detail "" stands for Python's None.
//
// py: agent_decisions/jev.py JevError
type JevError struct {
	Reason string
	Detail string
	Phase  string
}

func (e *JevError) Error() string {
	if e.Detail != "" {
		return e.Reason + ": " + e.Detail
	}
	return e.Reason
}

func (e *JevError) PyType() string   { return "JevError" }
func (e *JevError) PyModule() string { return "agent_decisions.jev" }

func init() { pyerr.Register("JevError", "Exception") }

// LoadKey returns the API key from TYPESAFE_API_KEY, else from the key file.
//
// py: agent_decisions/jev.py load_key
func LoadKey() (string, error) {
	key := pystr.Strip(os.Getenv("TYPESAFE_API_KEY"))
	if key == "" {
		path, err := keyFile()
		if err != nil {
			return "", err
		}
		key, err = pathlib.ReadText(path)
		if err != nil {
			if !errors.As(err, new(*pyerr.OSError)) {
				return "", err
			}
			key = ""
		}
		key = pystr.Strip(key)
	}
	if key == "" {
		return "", &JevError{Reason: "no_key"}
	}
	return key, nil
}

// PostFunc is the transport that Ask calls: Post, or a fake in tests.
type PostFunc func(body []byte, key string, timeout time.Duration) ([]byte, error)

// Post sends body to Jev and returns the response body. urllib's socket timeout restarts on each read, so
// Python bounds the complete exchange with a thread; this bounds it the same way.
//
// py: agent_decisions/jev.py post
func Post(body []byte, key string, timeout time.Duration) ([]byte, error) {
	endpoint, roundTripper := url, transport
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := post(body, key, timeout, endpoint, roundTripper)
		done <- result{raw, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.raw, r.err
	case <-timer.C:
		return nil, &JevError{Reason: "timeout"}
	}
}

// post makes the request with the method, headers and body that urllib sends, and maps each failure to a
// JevError as Python does.
//
// py: agent_decisions/jev.py _post
func post(body []byte, key string, timeout time.Duration, endpoint string, roundTripper http.RoundTripper) ([]byte, error) {
	authorization, err := latin1("Bearer " + key) // http.client encodes header values as latin-1
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &JevError{Reason: "unreachable"}
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Python-urllib/3.15")
	request.Header.Set("Accept-Encoding", "identity") // urllib never asks for a compressed body
	request.Close = true                              // urllib sends Connection: close
	client := &http.Client{Transport: roundTripper, Timeout: timeout, CheckRedirect: checkRedirect}
	response, err := client.Do(request)
	if err != nil {
		return nil, transportError(err, "request")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 { // urllib raises HTTPError for these
		detail := ""
		if response.StatusCode == 400 {
			if data, err := io.ReadAll(io.LimitReader(response.Body, 4096)); err == nil {
				if detail, err = validationDetail(data); err != nil {
					return nil, err
				}
			} // else Python keeps the HTTP failure: never echo an unknown response body.
		}
		return nil, &JevError{Reason: "http_" + strconv.Itoa(response.StatusCode), Detail: detail}
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, transportError(err, "response_body")
	}
	return raw, nil
}

// checkRedirect follows a redirect as urllib does for POST: 301, 302 and 303 become a GET, and 307 and
// 308 (which keep the POST) stop with that status, as urllib's HTTPError does. urllib sends no Referer
// and closes every connection. Go keeps its rule of dropping Authorization on a redirect to another
// host, where urllib forwards it.
func checkRedirect(request *http.Request, via []*http.Request) error {
	if request.Method != http.MethodGet || len(via) >= 10 {
		return http.ErrUseLastResponse
	}
	request.Header.Del("Referer")
	request.Close = true
	return nil
}

// transportError maps a failed exchange: a timeout is "timeout", any other failure "unreachable".
func transportError(err error, phase string) error {
	var netError net.Error
	if errors.As(err, &netError) && netError.Timeout() {
		return &JevError{Reason: "timeout"}
	}
	return &JevError{Reason: "unreachable", Phase: phase}
}

// validationDetail returns "max_tokens_exceeded" when a 400 body reports Jev's context limit, and "" for
// any other body. Only RecursionError escapes, as in Python.
func validationDetail(data []byte) (string, error) {
	value, err := loadsBytes(data)
	if err != nil {
		if errors.As(err, new(*json.RecursionError)) {
			return "", err
		}
		return "", nil
	}
	response, ok := value.(*json.Object)
	if !ok {
		return "", nil // AttributeError on .get, which Python catches
	}
	validation, ok := response.Value("detail").(*json.Object)
	if ok && validation.Value("error_type") == "max_tokens_exceeded" {
		return "max_tokens_exceeded", nil
	}
	return "", nil
}

// latin1 returns s encoded as latin-1, in a Go string of those bytes, or the UnicodeEncodeError that
// str.encode("latin-1") raises.
//
// py: Objects/unicodeobject.c unicode_encode_ucs1 (Python 3.15)
func latin1(s string) (string, error) {
	var out []byte
	for index, offset := 0, 0; offset < len(s); index++ {
		r, size := pystr.DecodeRune(s[offset:])
		if r > 0xFF {
			end := index + 1
			for rest := s[offset+size:]; rest != ""; end++ {
				next, width := pystr.DecodeRune(rest)
				if next <= 0xFF {
					break
				}
				rest = rest[width:]
			}
			if end-index == 1 {
				return "", &pyerr.Error{Class: "UnicodeEncodeError", Msg: fmt.Sprintf(
					"'latin-1' codec can't encode character '%s' in position %d: ordinal not in range(256)",
					escapeRune(r), index)}
			}
			return "", &pyerr.Error{Class: "UnicodeEncodeError", Msg: fmt.Sprintf(
				"'latin-1' codec can't encode characters in position %d-%d: ordinal not in range(256)", index, end-1)}
		}
		out = append(out, byte(r))
		offset += size
	}
	return string(out), nil
}

// escapeRune returns the \uXXXX or \UXXXXXXXX escape that an encode error message shows for r.
func escapeRune(r rune) string {
	if r <= 0xFFFF {
		return fmt.Sprintf(`\u%04x`, r)
	}
	return fmt.Sprintf(`\U%08x`, r)
}

// contextLimitError reports the JSON size of each part of an oversized request, without its text.
//
// py: agent_decisions/jev.py _context_limit_error
func contextLimitError(state, questions *json.Object, attemptCount, requestBytes int) error {
	bundle, err := objectGet(state, "evidence")
	if err != nil {
		return err
	}
	var sizes []string
	size := func(name string, value any) error {
		text, err := json.Dumps(value, json.Options{})
		if err != nil {
			return err
		}
		sizes = append(sizes, fmt.Sprintf("%s=%d", name, len(text))) // ensure_ascii text: bytes are characters
		return nil
	}
	for _, name := range []string{"user_messages", "tool_calls_newest_first", "source_files",
		"architecture_history", "brief"} {
		if err := size(name, getOr(bundle, name)); err != nil {
			return err
		}
	}
	for _, name := range []string{"decisions", "pending_input"} {
		if err := size(name, getOr(state, name)); err != nil {
			return err
		}
	}
	longest := 0
	for _, id := range questions.Keys() {
		text, err := json.Dumps(questions.Value(id), json.Options{})
		if err != nil {
			return err
		}
		longest = max(longest, len(text))
	}
	sizes = append(sizes, fmt.Sprintf("longest_question=%d", longest))
	if err := size("state", state); err != nil {
		return err
	}
	return &JevError{Reason: "http_400", Detail: fmt.Sprintf(
		"max_tokens_exceeded; attempt_count=%d; request_bytes=%d; JSON characters: %s",
		attemptCount, requestBytes, strings.Join(sizes, ", "))}
}

// objectGet returns d.get(key, {}) as a dict, or the AttributeError that a later .get raises in Python.
func objectGet(d *json.Object, key string) (*json.Object, error) {
	value, ok := d.Get(key)
	if !ok {
		return &json.Object{}, nil
	}
	object, ok := value.(*json.Object)
	if !ok {
		return nil, &pyerr.Error{Class: "AttributeError",
			Msg: fmt.Sprintf("'%s' object has no attribute 'get'", json.TypeName(value))}
	}
	return object, nil
}

// getOr returns d.get(key, {}).
func getOr(d *json.Object, key string) any {
	if value, ok := d.Get(key); ok {
		return value
	}
	return &json.Object{}
}

// Ask sends state and questions to Jev and returns the validated answers. On Jev's context limit it
// retries once with shortened observation history, which it stores in state["evidence"].
//
// py: agent_decisions/jev.py ask
func Ask(state, questions *json.Object, key string, post PostFunc, metrics *json.Object) (*json.Object, error) {
	deadline := now().Add(time.Duration(policy.TimeoutS * float64(time.Second)))
	started := now()
	raw, err := exchange(state, questions, key, post, metrics, deadline)
	if metrics != nil {
		metrics.Set("api_ms", milliseconds(now().Sub(started)))
	}
	if err != nil {
		return nil, err
	}
	return ValidateResponse(raw, questions, metrics)
}

// ValidateResponse decodes a Jev-compatible response and validates each requested answer.
func ValidateResponse(raw []byte, questions, metrics *json.Object) (*json.Object, error) {
	value, err := loadsBytes(raw)
	if err != nil {
		if errors.As(err, new(*json.RecursionError)) {
			return nil, err
		}
		return nil, malformed()
	}
	response, ok := value.(*json.Object)
	if !ok {
		return nil, malformed()
	}
	answers, ok := response.Value("answers").(*json.Object)
	if !ok {
		return nil, malformed()
	}
	for _, id := range questions.Keys() {
		var err error = &pyerr.Error{Class: "KeyError", Msg: pystr.Repr(id)}
		if answer, ok := answers.Get(id); ok {
			err = check(answer, questions.Value(id))
		}
		if err != nil {
			if !pyerr.IsInstance(err, "ValueError", "KeyError", "TypeError") {
				return nil, err
			}
			label := "question"
			if questionLabel(id) {
				label = id
			}
			reason := "missing or invalid fields"
			if pyerr.IsInstance(err, "ValueError") {
				reason = err.Error()
			}
			return nil, &JevError{Reason: "malformed", Detail: label + ": " + reason}
		}
	}
	if metrics != nil {
		if model, ok := response.Value("model").(string); ok {
			metrics.Set("model", pystr.SliceTo(model, 100))
		}
		if usage, ok := response.Value("usage").(*json.Object); ok {
			kept := &json.Object{}
			for _, name := range usage.Keys() {
				value := usage.Value(name)
				if _, ok := value.(bool); ok {
					continue
				}
				number, isNumber, err := finite(value)
				if err != nil {
					return nil, err
				}
				if isNumber && number >= 0 {
					kept.Set(name, value)
				}
			}
			metrics.Set("usage", kept)
		}
	}
	return answers, nil
}

func malformed() error {
	return &JevError{Reason: "malformed", Detail: "response JSON or answers object"}
}

// questionLabel reports whether a question id is safe to name in a diagnostic.
func questionLabel(id string) bool {
	if id == "" || pystr.Len(id) > 64 {
		return id == "" // all() of no characters is true
	}
	for _, c := range []byte(id) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("_-.", c) >= 0) {
			return false
		}
	}
	return true
}

// exchange serializes the request and posts it, retrying once with shorter evidence on Jev's context
// limit. It is the try block of ask.
//
// py: agent_decisions/jev.py ask
func exchange(state, questions *json.Object, key string, post PostFunc, metrics *json.Object,
	deadline time.Time) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		serializationStarted := now()
		text, err := json.Dumps(json.NewObject("model", policy.Model, "state", state, "questions", questions),
			json.Options{})
		if err != nil {
			return nil, err
		}
		body := []byte(text)
		if metrics != nil {
			if _, ok := metrics.Get("initial_request_bytes"); !ok {
				metrics.Set("initial_request_bytes", json.IntOf(int64(len(body))))
			}
			metrics.Set("request_bytes", json.IntOf(int64(len(body))))
			metrics.Set("question_count", json.IntOf(int64(questions.Len())))
			metrics.Set("requested_model", policy.Model)
			metrics.Set("attempt_count", json.IntOf(int64(attempt+1)))
			previous, _ := metrics.Value("serialization_ms").(float64)
			metrics.Set("serialization_ms", previous+milliseconds(now().Sub(serializationStarted)))
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return nil, &JevError{Reason: "timeout"}
		}
		raw, err := post(body, key, remaining)
		if err == nil {
			return raw, nil
		}
		var jevError *JevError
		if !errors.As(err, &jevError) {
			return nil, err
		}
		limited := jevError.Reason == "http_400" && jevError.Detail == "max_tokens_exceeded"
		if attempt > 0 || !limited {
			if limited {
				return nil, contextLimitError(state, questions, attempt+1, len(body))
			}
			return nil, err
		}
		query, err := retryQuery(state)
		if err != nil {
			return nil, err
		}
		bundle, err := evidenceBundle(state)
		if err != nil {
			return nil, err
		}
		shorter, err := evidence.ShortenObservations(bundle, query, contractPaths(state.Value("contracts")))
		if err != nil {
			return nil, err
		}
		if shorter == nil {
			return nil, contextLimitError(state, questions, attempt+1, len(body))
		}
		state.Set("evidence", shorter)
	}
}

// retryQuery returns str(state.get("decision") or state.get("final_message") or
// json.dumps(state.get("pending_input", {}))).
func retryQuery(state *json.Object) (string, error) {
	for _, name := range []string{"decision", "final_message"} {
		if value := state.Value(name); json.Truthy(value) {
			return display(value), nil
		}
	}
	return json.Dumps(getOr(state, "pending_input"), json.Options{})
}

// evidenceBundle returns state.get("evidence", {}) as the dict that shorten_observations copies.
func evidenceBundle(state *json.Object) (*json.Object, error) {
	value := getOr(state, "evidence")
	bundle, ok := value.(*json.Object)
	if !ok {
		return nil, &pyerr.Error{Class: "TypeError",
			Msg: fmt.Sprintf("'%s' object is not a mapping", json.TypeName(value))}
	}
	return bundle, nil
}

// contractPaths returns the elements of state.get("contracts", ()) that set() collects: a dict's keys, a
// list's strings, or a string's characters. Other elements cannot equal a str path.
func contractPaths(value any) []string {
	switch value := value.(type) {
	case *json.Object:
		return value.Keys()
	case []any:
		var paths []string
		for _, item := range value {
			if path, ok := item.(string); ok {
				paths = append(paths, path)
			}
		}
		return paths
	case string:
		var paths []string
		for offset := 0; offset < len(value); {
			_, size := pystr.DecodeRune(value[offset:])
			paths = append(paths, value[offset:offset+size])
			offset += size
		}
		return paths
	}
	return nil
}

func milliseconds(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

// check validates one answer against its question. A *pyerr.Error of class ValueError, KeyError or
// TypeError is a malformed answer; any other error escapes ask, as in Python.
//
// py: agent_decisions/jev.py _check
func check(answer, question any) error {
	answerType, err := subscript(answer, "type")
	if err != nil {
		return err
	}
	questionType, err := subscript(question, "type")
	if err != nil {
		return err
	}
	same, err := json.Equal(answerType, questionType)
	if err != nil {
		return err
	}
	if !same {
		return valueError("answer type")
	}
	if questionType == "noul" {
		noul, err := subscript(answer, "noul")
		if err != nil {
			return err
		}
		return probability(noul)
	}
	probabilities, err := subscript(answer, "probabilities")
	if err != nil {
		return err
	}
	choice, err := subscript(answer, "choice")
	if err != nil {
		return err
	}
	criteriaValue, err := subscript(question, "criteria")
	if err != nil {
		return err
	}
	criteria, ok := criteriaValue.(*json.Object)
	if !ok { // gates.py always sends a dict
		return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("criteria is a '%s', not a dict", json.TypeName(criteriaValue))}
	}
	if err := json.Unhashable(choice, "dict key"); err != nil {
		return err
	}
	choiceKey, isString := choice.(string)
	if _, inCriteria := criteria.Get(choiceKey); !isString || !inCriteria {
		return valueError("choice")
	}
	distribution, ok := probabilities.(*json.Object)
	if !ok || !sameKeys(distribution, criteria) {
		return valueError("choice")
	}
	confidence, err := subscript(answer, "confidence")
	if err != nil {
		return err
	}
	if err := probability(confidence); err != nil {
		return err
	}
	values := make([]float64, 0, distribution.Len())
	for _, name := range distribution.Keys() {
		value := distribution.Value(name)
		if err := probability(value); err != nil {
			return err
		}
		number, _, _ := finite(value) // probability accepted it
		values = append(values, number)
	}
	// Decimal totals 0.99 and 1.01 can lie one binary rounding step past 0.01.
	chosen, _, _ := finite(distribution.Value(choiceKey))
	if !isClose(fsum(values), 1.0, 0.01+ulpOne) || chosen != maxOf(values) {
		return valueError("choice distribution")
	}
	return nil
}

// ulpOne is math.ulp(1.0).
const ulpOne = 0x1p-52

// probability rejects anything but a finite int or float in [0, 1].
//
// py: agent_decisions/jev.py _probability
func probability(value any) error {
	if _, ok := value.(bool); ok {
		return valueError("probability")
	}
	number, isNumber, err := finite(value)
	if err != nil {
		return err
	}
	if !isNumber || !(0.0 <= number && number <= 1.0) {
		return valueError("probability")
	}
	return nil
}

// finite returns value as a float when it is an int or float for which math.isfinite holds. An int too
// large for a float is the OverflowError that math.isfinite raises, which ask does not catch.
func finite(value any) (number float64, ok bool, err error) {
	switch value := value.(type) {
	case float64:
		return value, !math.IsInf(value, 0) && !math.IsNaN(value), nil
	case json.Int:
		number, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return 0, false, &pyerr.Error{Class: "OverflowError", Msg: "int too large to convert to float"}
		}
		return number, true, nil
	}
	return 0, false, nil
}

func valueError(message string) error {
	return &pyerr.Error{Class: "ValueError", Msg: message}
}

// subscript returns value[key] for a str key, with the KeyError or TypeError that Python raises.
func subscript(value any, key string) (any, error) {
	switch v := value.(type) {
	case *json.Object:
		item, ok := v.Get(key)
		if !ok {
			return nil, &pyerr.Error{Class: "KeyError", Msg: pystr.Repr(key)}
		}
		return item, nil
	case []any:
		return nil, &pyerr.Error{Class: "TypeError", Msg: "list indices must be integers or slices, not str"}
	case string:
		return nil, &pyerr.Error{Class: "TypeError", Msg: "string indices must be integers, not 'str'"}
	}
	return nil, &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("'%s' object is not subscriptable", json.TypeName(value))}
}

func sameKeys(a, b *json.Object) bool {
	if a.Len() != b.Len() {
		return false
	}
	for _, key := range a.Keys() {
		if _, ok := b.Get(key); !ok {
			return false
		}
	}
	return true
}

func maxOf(values []float64) float64 {
	best := math.Inf(-1)
	for _, value := range values {
		best = max(best, value)
	}
	return best
}

// isClose ports math.isclose(a, b, rel_tol=0, abs_tol=absTol) for finite a and b.
//
// py: Modules/mathmodule.c math_isclose_impl (Python 3.15)
func isClose(a, b, absTol float64) bool {
	return a == b || math.Abs(b-a) <= absTol
}

// fsum ports math.fsum for finite values whose partial sums cannot overflow, such as probabilities:
// Shewchuk's exact partial sums, rounded half-even.
//
// py: Modules/mathmodule.c math_fsum (Python 3.15)
func fsum(values []float64) float64 {
	var partials []float64
	for _, x := range values {
		i := 0
		for _, y := range partials {
			if math.Abs(x) < math.Abs(y) {
				x, y = y, x
			}
			hi := x + y
			lo := y - (hi - x)
			if lo != 0.0 {
				partials[i] = lo
				i++
			}
			x = hi
		}
		partials = append(partials[:i], x)
	}
	n := len(partials)
	if n == 0 {
		return 0.0
	}
	n--
	hi, lo := partials[n], 0.0
	for n > 0 {
		x := hi
		n--
		y := partials[n]
		hi = x + y
		lo = y - (hi - x)
		if lo != 0.0 {
			break
		}
	}
	// Make half-even rounding work across multiple partials.
	if n > 0 && (lo < 0.0 && partials[n-1] < 0.0 || lo > 0.0 && partials[n-1] > 0.0) {
		y := lo * 2.0
		x := hi + y
		if y == x-hi {
			hi = x
		}
	}
	return hi
}

// loadsBytes ports json.loads for bytes: the encoding comes from the first bytes, and decoding keeps
// lone surrogates (surrogatepass). A decoding failure is the ValueError Python raises.
//
// py: Lib/json/__init__.py loads, detect_encoding (Python 3.15)
func loadsBytes(data []byte) (any, error) {
	text, err := decodeJSONBytes(data)
	if err != nil {
		return nil, err
	}
	return json.Loads(text)
}

func decodeJSONBytes(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0, 0, 0xFE, 0xFF}):
		return decodeUnits(b[4:], 4, binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE, 0, 0}):
		return decodeUnits(b[4:], 4, binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUnits(b[2:], 2, binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUnits(b[2:], 2, binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return decodeUTF8(b[3:])
	case len(b) >= 4 && b[0] == 0 && b[1] != 0:
		return decodeUnits(b, 2, binary.BigEndian)
	case len(b) >= 4 && b[0] == 0:
		return decodeUnits(b, 4, binary.BigEndian)
	case len(b) >= 4 && b[1] == 0 && (b[2] != 0 || b[3] != 0):
		return decodeUnits(b, 2, binary.LittleEndian)
	case len(b) >= 4 && b[1] == 0:
		return decodeUnits(b, 4, binary.LittleEndian)
	case len(b) == 2 && b[0] == 0:
		return decodeUnits(b, 2, binary.BigEndian)
	case len(b) == 2 && b[1] == 0:
		return decodeUnits(b, 2, binary.LittleEndian)
	}
	return decodeUTF8(b)
}

// decodeUTF8 accepts UTF-8 with encoded surrogates, which a Go string keeps as WTF-8.
func decodeUTF8(b []byte) (string, error) {
	text := string(b)
	for offset := 0; offset < len(text); {
		_, size := pystr.DecodeRune(text[offset:])
		if size == 1 && text[offset] >= 0x80 {
			return "", decodeError("utf-8")
		}
		offset += size
	}
	return text, nil
}

// decodeUnits decodes UTF-16 or UTF-32 code units with surrogatepass. A UTF-16 surrogate pair is one code
// point; any other surrogate stays a lone surrogate.
func decodeUnits(b []byte, width int, order binary.ByteOrder) (string, error) {
	if len(b)%width != 0 {
		return "", decodeError("utf-16")
	}
	var out []byte
	for i := 0; i < len(b); i += width {
		var r rune
		if width == 4 {
			code := order.Uint32(b[i:])
			if code > utf8.MaxRune {
				return "", decodeError("utf-32")
			}
			r = rune(code)
		} else {
			r = rune(order.Uint16(b[i:]))
			if 0xD800 <= r && r < 0xDC00 && i+2 < len(b) {
				if low := rune(order.Uint16(b[i+2:])); 0xDC00 <= low && low < 0xE000 {
					r = 0x10000 + (r-0xD800)<<10 + (low - 0xDC00)
					i += 2
				}
			}
		}
		out = appendWTF8(out, r)
	}
	return string(out), nil
}

// appendWTF8 appends r, writing a lone surrogate as its 3-byte generalized UTF-8 form.
func appendWTF8(out []byte, r rune) []byte {
	if 0xD800 <= r && r < 0xE000 {
		return append(out, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
	return utf8.AppendRune(out, r)
}

// decodeError is the UnicodeDecodeError, a ValueError, that json.loads lets through for undecodable bytes.
// Its message is not modelled: ask and post catch it without showing it.
func decodeError(codec string) error {
	return &pyerr.Error{Class: "UnicodeDecodeError", Msg: "'" + codec + "' codec can't decode bytes"}
}

// display returns str(v) for a decoded JSON value.
func display(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	return json.Repr(v)
}
