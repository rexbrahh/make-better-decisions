// Package ledger is the append-only per-session ledger and the per-call verdict files.
//
// Rows are byte-identical to the Python ledger's, so either implementation reads the other's rows with the
// same key. The policy functions that read the ledger (InstallationPolicy, ReviewMode,
// SessionReviewException, FailOpen) live here as well; see policy.go.
//
// py: agent_decisions/ledger.py
package ledger

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
	"github.com/rexbrahh/make-better-decisions/internal/py/pyerr"
	"github.com/rexbrahh/make-better-decisions/internal/py/str"
)

// Side effects that tests replace.
var (
	now              = time.Now
	random io.Reader = rand.Reader
)

// safe maps every code point outside [A-Za-z0-9_-] to "_" and keeps the first 128, or returns "_" for
// an empty name.
//
// py: agent_decisions/ledger.py _safe
func safe(name string) string {
	var out []byte
	for count := 0; name != "" && count < 128; count++ {
		r, width := str.DecodeRune(name)
		name = name[width:]
		if 'A' <= r && r <= 'Z' || 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '_' || r == '-' {
			out = append(out, byte(r))
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}

// py: agent_decisions/ledger.py _ENTRY_FIELD, _SEAL_FIELD, _SEAL_VERSION, _KEY_BYTES
const (
	entryField  = "entry"
	sealField   = "_ledger_seal"
	sealVersion = "hmac-sha256-v1"
	keyBytes    = 32
)

// py: agent_decisions/ledger.py _key_file
func keyFile() (string, error) {
	dir, err := policy.StateDir()
	return pathlib.Join(dir, "ledger.key"), err
}

// py: agent_decisions/ledger.py _key_lock_file
func keyLockFile() (string, error) {
	dir, err := policy.StateDir()
	return pathlib.Join(dir, "ledger.key.lock"), err
}

// py: agent_decisions/ledger.py _session_file
func sessionFile(sessionID string) (string, error) {
	dir, err := policy.StateDir()
	return pathlib.Join(pathlib.Join(dir, "sessions"), safe(sessionID)+".jsonl"), err
}

// readExactKey returns the key at path, or nil when it cannot be read or is not keyBytes long.
//
// py: agent_decisions/ledger.py _read_exact_key
func readExactKey(path string) []byte {
	key, err := os.ReadFile(path)
	if err != nil || len(key) != keyBytes {
		return nil
	}
	return key
}

// writeNewKey writes a fresh random key to a temporary file, syncs it, and renames it over path.
//
// py: agent_decisions/ledger.py _write_new_key
func writeNewKey(path string) ([]byte, error) {
	key := make([]byte, keyBytes)
	if _, err := io.ReadFull(random, key); err != nil {
		return nil, pyerr.NewOSError(err)
	}
	temporary := pathlib.Join(pathlib.Parent(path), fmt.Sprintf(".%s.%d.%d.tmp", "ledger.key", os.Getpid(), now().UnixNano()))
	if err := installKey(temporary, path, key); err != nil {
		if unlinkErr := syscall.Unlink(temporary); unlinkErr != nil && unlinkErr != syscall.ENOENT {
			return nil, pyerr.NewOSError(unlinkErr, temporary)
		}
		return nil, err
	}
	return key, nil
}

// installKey is the try block of _write_new_key.
//
// py: agent_decisions/ledger.py _write_new_key
func installKey(temporary, path string, key []byte) error {
	fd, err := openFD(temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = writeAll(fd, key)
	if err == nil {
		err = fsync(fd)
	}
	if closeErr := closeFD(fd); closeErr != nil {
		return closeErr
	}
	if err != nil {
		return err
	}
	if err := syscall.Rename(temporary, path); err != nil {
		return pyerr.NewOSError(err, temporary, path)
	}
	if err := syscall.Chmod(path, 0o600); err != nil {
		return pyerr.NewOSError(err, path)
	}
	return nil
}

// key returns the ledger key. Without create it returns nil for a missing or damaged key. With create it
// makes the key under an exclusive lock when none exists, and fails on a damaged one.
//
// py: agent_decisions/ledger.py _key
func key(create bool) ([]byte, error) {
	path, err := keyFile()
	if err != nil {
		return nil, err
	}
	if !create {
		return readExactKey(path), nil
	}

	if err := pathlib.Mkdir(pathlib.Parent(path), 0o700, true); err != nil {
		return nil, err
	}
	lockPath, err := keyLockFile()
	if err != nil {
		return nil, err
	}
	lockFD, err := openFD(lockPath, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return nil, err
	}
	key, err := lockedKey(lockFD, path)
	if closeErr := closeFD(lockFD); closeErr != nil {
		return nil, closeErr
	}
	return key, err
}

// lockedKey is the try block of _key.
//
// py: agent_decisions/ledger.py _key
func lockedKey(lockFD int, path string) ([]byte, error) {
	if err := flock(lockFD, syscall.LOCK_EX); err != nil {
		return nil, err
	}
	if key := readExactKey(path); key != nil {
		if err := syscall.Chmod(path, 0o600); err != nil {
			return nil, pyerr.NewOSError(err, path)
		}
		return key, nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil, &pyerr.OSError{Class: "OSError", Msg: "corrupt ledger key"}
	}
	return writeNewKey(path)
}

// canonical returns the bytes that the seal covers.
//
// py: agent_decisions/ledger.py _canonical
func canonical(entry *json.Object) ([]byte, error) {
	text, err := json.Dumps(entry, json.Options{SortKeys: true, Compact: true})
	return []byte(text), err
}

// py: agent_decisions/ledger.py _seal
func seal(entry *json.Object, key []byte) (string, error) {
	data, err := canonical(entry)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return sealVersion + ":" + hex.EncodeToString(mac.Sum(nil)), nil
}

// py: agent_decisions/ledger.py _signed_row
func signedRow(entry *json.Object, key []byte) (*json.Object, error) {
	sealed, err := seal(entry, key)
	if err != nil {
		return nil, err
	}
	return json.NewObject(entryField, entry, sealField, sealed), nil
}

// verifiedEntry returns the entry of row when its seal matches and it belongs to sessionID, else nil.
// A seal with a non-ASCII character is the TypeError that hmac.compare_digest raises.
//
// py: agent_decisions/ledger.py _verified_entry
func verifiedEntry(row *json.Object, sessionID string, key []byte) (*json.Object, error) {
	entry, isDict := row.Value(entryField).(*json.Object)
	got, isStr := row.Value(sealField).(string)
	if !isDict || !isStr {
		return nil, nil
	}
	if entry.Value("session_id") != sessionID {
		return nil, nil
	}
	want, err := seal(entry, key)
	if err != nil {
		return nil, err
	}
	for i := range len(got) {
		if got[i] >= 0x80 {
			return nil, &pyerr.Error{Class: "TypeError", Msg: "comparing strings with non-ASCII characters is not supported"}
		}
	}
	if !hmac.Equal([]byte(got), []byte(want)) {
		return nil, nil
	}
	return entry, nil
}

// Append seals entry and appends it to the file of entry["session_id"] under an exclusive lock, because
// parallel subagents share one session file.
//
// py: agent_decisions/ledger.py append
func Append(entry *json.Object) error {
	key, err := key(true)
	if err != nil {
		return err
	}
	if key == nil {
		return &pyerr.OSError{Class: "OSError", Msg: "missing ledger key"}
	}
	sessionValue, err := item(entry, "session_id")
	if err != nil {
		return err
	}
	sessionID, ok := sessionValue.(string)
	if !ok {
		return &pyerr.Error{Class: "TypeError", Msg: fmt.Sprintf("expected string or bytes-like object, got '%s'", typeName(sessionValue))}
	}
	path, err := sessionFile(sessionID)
	if err != nil {
		return err
	}
	if err := pathlib.Mkdir(pathlib.Parent(path), 0o700, true); err != nil {
		return err
	}
	row, err := signedRow(entry, key)
	if err != nil {
		return err
	}
	data, err := json.Dumps(row, json.Options{SortKeys: true})
	if err != nil {
		return err
	}
	data += "\n"
	fd, err := openFD(path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	err = flock(fd, syscall.LOCK_EX)
	if err == nil {
		err = writeAll(fd, []byte(data))
	}
	if closeErr := closeFD(fd); closeErr != nil { // closing releases the lock
		return closeErr
	}
	return err
}

// Entries returns the verified entries of a session in file order. A nil cacheLimit is Python's None.
// With a cacheLimit, only the newest cacheLimit edit, question, plan and stop entries keep their "state"
// and "chunks" payloads. A line that is not JSON is skipped: it may still be being written.
//
// py: agent_decisions/ledger.py entries
func Entries(sessionID string, cacheLimit *int) ([]*json.Object, error) {
	if cacheLimit != nil && *cacheLimit < 0 {
		return nil, &pyerr.Error{Class: "ValueError", Msg: "cache_limit must be non-negative"}
	}
	key, err := key(false)
	if err != nil || key == nil {
		return nil, err
	}
	path, err := sessionFile(sessionID)
	if err != nil {
		return nil, err
	}
	stream, err := openText(path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, nil
		}
		return nil, err
	}
	defer stream.Close()

	var rows, cached []*json.Object
	lines := bufio.NewReader(stream)
	for {
		line, err := readLine(lines)
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, readError(err)
		}
		value, err := json.Loads(json.DecodeUTF8(line))
		if err != nil {
			var decodeErr *json.DecodeError
			var digitsErr *json.IntDigitsError
			if errors.As(err, &decodeErr) || errors.As(err, &digitsErr) {
				continue // a ValueError: a line still being written
			}
			return nil, err
		}
		row, isDict := value.(*json.Object)
		if !isDict {
			continue
		}
		entry, err := verifiedEntry(row, sessionID, key)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			continue
		}
		rows = append(rows, entry)
		if cacheLimit == nil {
			continue
		}
		reviewed, err := contains(entry.Value("gate"), "edit", "question", "plan", "stop")
		if err != nil {
			return nil, err
		}
		if reviewed {
			cached = append(cached, entry)
			if len(cached) > *cacheLimit {
				expired := cached[0]
				cached = cached[1:]
				expired.Delete("state")
				expired.Delete("chunks")
			}
		}
	}
}

// openText opens path for reading as open() does: a directory is an IsADirectoryError.
func openText(path string) (*os.File, error) {
	fd, err := openFD(path, syscall.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err == nil && stat.Mode&syscall.S_IFMT == syscall.S_IFDIR {
		syscall.Close(fd)
		return nil, pyerr.NewOSError(syscall.EISDIR, path)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// readLine returns the next line without its terminator, as a text file in universal newlines mode
// splits it: "\n", "\r" and "\r\n" each end a line. Those bytes never occur inside a multi-byte UTF-8
// sequence, so splitting before decoding gives the same lines as decoding first. It returns io.EOF when
// no line is left.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		b, err := r.ReadByte()
		if err == io.EOF && len(line) > 0 {
			return line, nil
		}
		if err != nil {
			return nil, err
		}
		switch b {
		case '\n':
			return line, nil
		case '\r':
			if next, err := r.Peek(1); err == nil && next[0] == '\n' {
				r.ReadByte()
			}
			return line, nil
		}
		line = append(line, b)
	}
}

// VerdictName returns the file name of the verdict for a tool call.
//
// py: agent_decisions/ledger.py verdict_name
func VerdictName(toolUseID string) string {
	return safe(toolUseID) + ".txt"
}

// WriteVerdict writes text to the verdict file of a tool call through a temporary file and returns the
// verdict path. A lone surrogate in text is written as its \udXXX escape.
//
// py: agent_decisions/ledger.py write_verdict
func WriteVerdict(toolUseID, text string) (string, error) {
	dir, err := policy.StateDir()
	if err != nil {
		return "", err
	}
	directory := pathlib.Join(dir, "verdicts")
	path := pathlib.Join(directory, VerdictName(toolUseID))
	if err := pathlib.Mkdir(directory, 0o700, true); err != nil {
		return "", err
	}
	temporary := pathlib.Join(directory, safe(toolUseID)+".tmp")
	fd, err := openFD(temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC, 0o666)
	if err != nil {
		return "", err
	}
	err = writeAll(fd, backslashReplace(text))
	if closeErr := closeFD(fd); closeErr != nil {
		return "", closeErr
	}
	if err != nil {
		return "", err
	}
	if err := syscall.Rename(temporary, path); err != nil {
		return "", pyerr.NewOSError(err, temporary, path)
	}
	return path, nil
}

// py: agent_decisions/ledger.py _architecture_session
func architectureSession(repoID string) (string, error) {
	data, err := pyerr.EncodeUTF8(repoID)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(data))
	return "architecture-" + hex.EncodeToString(digest[:]), nil
}

// ArchitecturePath returns the ledger file that holds the architecture of a repository.
//
// py: agent_decisions/ledger.py architecture_path
func ArchitecturePath(repoID string) (string, error) {
	session, err := architectureSession(repoID)
	if err != nil {
		return "", err
	}
	return sessionFile(session)
}

// ArchitectureEntries returns the newest 16 architecture entries of a repository.
//
// py: agent_decisions/ledger.py architecture_entries
func ArchitectureEntries(repoID string) ([]*json.Object, error) {
	session, err := architectureSession(repoID)
	if err != nil {
		return nil, err
	}
	rows, err := Entries(session, nil)
	if err != nil {
		return nil, err
	}
	rows = slices.DeleteFunc(rows, func(row *json.Object) bool {
		return row.Value("gate") != "architecture" || row.Value("repo_id") != repoID
	})
	return rows[max(len(rows)-16, 0):], nil
}

// RememberArchitecture records each new advisory or binding decision with contracts as an architecture
// entry of its repository. Call it only for completed receipts verified by the host-context loader.
//
// py: agent_decisions/ledger.py remember_architecture
func RememberArchitecture(repoID string, decisions []*json.Object) error {
	if !slices.ContainsFunc(decisions, func(entry *json.Object) bool { return json.Truthy(entry.Value("contracts")) }) {
		return nil
	}
	session, err := architectureSession(repoID)
	if err != nil {
		return err
	}
	rows, err := Entries(session, nil)
	if err != nil {
		return err
	}
	var known []any // a set: every member is hashable
	for _, row := range rows {
		receipt := row.Value("source_receipt")
		if err := hashable(receipt, "a set element"); err != nil {
			return err
		}
		known = append(known, receipt)
	}
	for _, entry := range decisions {
		skip, err := skipDecision(entry, repoID, known)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		observed, err := observation(session, repoID, entry)
		if err != nil {
			return err
		}
		if err := Append(observed); err != nil {
			return err
		}
		known = append(known, entry.Value("receipt_id"))
	}
	return nil
}

// skipDecision is the `continue` condition of remember_architecture, with its short-circuit order.
//
// py: agent_decisions/ledger.py remember_architecture
func skipDecision(entry *json.Object, repoID string, known []any) (bool, error) {
	decided, err := contains(entry.Value("action"), "advisory", "binding")
	if err != nil || !decided || !json.Truthy(entry.Value("contracts")) || !json.Truthy(entry.Value("receipt_id")) ||
		entry.Value("repo_id") != repoID {
		return true, err
	}
	return contains(entry.Value("receipt_id"), known...)
}

// observation builds the architecture entry that remember_architecture appends, evaluating its parts in
// the Python order so the first error is the same.
//
// py: agent_decisions/ledger.py remember_architecture
func observation(session, repoID string, entry *json.Object) (*json.Object, error) {
	state, err := get(entry, "state", &json.Object{})
	if err != nil {
		return nil, err
	}
	scope, err := get(state, "scope", &json.Object{})
	if err != nil {
		return nil, err
	}
	decisionText, err := get(entry, "decision", "")
	if err == nil {
		decisionText, err = head(decisionText, 500)
	}
	if err != nil {
		return nil, err
	}
	pick, err := item(entry, "pick")
	if err != nil {
		return nil, err
	}
	options, err := get(state, "options", &json.Object{})
	if err != nil {
		return nil, err
	}
	approach, err := optionOf(options, pick)
	if err == nil {
		approach, err = head(approach, 1_000)
	}
	if err != nil {
		return nil, err
	}
	scopes := &json.Object{}
	for _, name := range []string{"paths", "invariants"} {
		values, err := get(scope, name, []any{})
		if err == nil {
			values, err = head(values, 8)
		}
		if err != nil {
			return nil, err
		}
		clipped := []any{}
		for _, value := range iterate(values) {
			value, err := head(value, 200)
			if err != nil {
				return nil, err
			}
			clipped = append(clipped, value)
		}
		scopes.Set(name, clipped)
	}
	return json.NewObject(
		"session_id", session, "gate", "architecture", "action", "observed", "repo_id", repoID,
		"source_receipt", entry.Value("receipt_id"), "time", entry.Value("time"), "decision", decisionText,
		"pick", pick, "selected_approach", approach, "scope", scopes, "contracts", entry.Value("contracts"),
	), nil
}

// optionOf returns options.get(pick, ""): the attribute lookup fails before the key is hashed.
func optionOf(options any, pick any) (any, error) {
	if _, err := get(options, "", nil); err != nil {
		return nil, err
	}
	if err := hashable(pick, "a dict key"); err != nil {
		return nil, err
	}
	name, isStr := pick.(string)
	if !isStr {
		return "", nil // a dict with str keys holds no other key
	}
	return get(options, name, "")
}

// Prune removes session and verdict files older than policy.RetainDays. The installation policy and the
// architecture files outlive chat retention.
//
// py: agent_decisions/ledger.py prune
func Prune() error {
	cutoff := float64(now().UnixNano())/1e9 - policy.RetainDays*86_400
	dir, err := policy.StateDir()
	if err != nil {
		return err
	}
	for _, sub := range []string{"sessions", "verdicts"} {
		directory := pathlib.Join(dir, sub)
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			continue
		}
		names, err := listDir(directory)
		if err != nil {
			return err
		}
		for _, name := range names {
			if sub == "sessions" && (name == "installation.jsonl" || strings.HasPrefix(name, "architecture-")) {
				continue // installation policy and architectural context outlive chat retention
			}
			path := pathlib.Join(directory, name)
			info, err := os.Stat(path)
			if errors.Is(err, syscall.ENOENT) {
				continue // another session pruned it first
			}
			if err != nil {
				return pyerr.NewOSError(err, path)
			}
			modified := info.ModTime()
			if float64(modified.Unix())+float64(modified.Nanosecond())*1e-9 >= cutoff {
				continue
			}
			if err := syscall.Unlink(path); err != nil && err != syscall.ENOENT {
				return pyerr.NewOSError(err, path)
			}
		}
	}
	return nil
}

// listDir returns the names in directory in directory order, as Path.iterdir yields them.
func listDir(directory string) ([]string, error) {
	file, err := os.Open(directory)
	if err != nil {
		return nil, pyerr.NewOSError(err, directory)
	}
	defer file.Close()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return nil, pyerr.NewOSError(err, directory)
	}
	return names, nil
}
