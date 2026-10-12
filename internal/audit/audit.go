// Package audit writes a tamper-evident log of who did what: every
// inference request (who, which model, which backend, tokens, cost; never
// prompt or response content), failed logins, and admin actions.
//
// Each line is a JSON object carrying a sequence number and the SHA-256
// hash of the previous line, and its own hash covers both. Editing,
// deleting or reordering lines breaks the chain, which Verify detects. The
// chain continues across rotated files and restarts. Ship the files to
// write-once storage (or a SIEM) for the guarantee to mean much: anyone who
// can rewrite the whole file can rewrite the chain too.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Config says where the log goes.
type Config struct {
	// Path of the log file, or "-" for standard output (no rotation).
	Path string
	// MaxBytes rotates the file when it would grow past this: path.1 is
	// the newest old file, up to path.<MaxFiles>.
	MaxBytes int64
	MaxFiles int
}

// Logger appends events. Safe for concurrent use; a nil Logger discards.
type Logger struct {
	cfg  Config
	mu   sync.Mutex
	w    io.Writer
	f    *os.File // nil for stdout
	size int64
	seq  uint64
	prev string // hash of the last line written
	now  func() time.Time
	// OnError is told about write failures (the request is not failed).
	OnError func(error)
}

// errEmpty: the file has no entries (new, or just rotated).
var errEmpty = errors.New("no entries")

// genesis is the "previous hash" of the very first line.
const genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Open opens (or creates) the log and continues its chain.
func Open(cfg Config) (*Logger, error) {
	l := &Logger{cfg: cfg, prev: genesis, now: time.Now}
	if cfg.Path == "-" {
		l.w = os.Stdout
		return l, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, err
	}
	if seq, hash, err := lastEntry(cfg.Path); err == nil {
		l.seq, l.prev = seq, hash
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errEmpty) {
		return nil, fmt.Errorf("audit log %s: %w", cfg.Path, err)
	} else if seq, hash, err := lastEntry(cfg.Path + ".1"); err == nil {
		l.seq, l.prev = seq, hash // the live file was rotated away and not recreated yet
	}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) openFile() error {
	f, err := os.OpenFile(l.cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.w, l.size = f, f, st.Size()
	return nil
}

// lastEntry reads the sequence number and hash of a log's last line.
func lastEntry(path string) (uint64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	const tail = 1 << 20
	off := max(0, st.Size()-tail)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return 0, "", err
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	last := lines[len(lines)-1]
	if len(bytes.TrimSpace(last)) == 0 {
		return 0, "", errEmpty
	}
	var e struct {
		Seq  uint64 `json:"seq"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(last, &e); err != nil || e.Hash == "" {
		return 0, "", fmt.Errorf("last line is not an audit entry; refusing to break the chain (move the file aside): %v", err)
	}
	return e.Seq, e.Hash, nil
}

// Log appends one event with its fields. Field names seq, time, event,
// prev and hash are the log's own.
func (l *Logger) Log(event string, fields map[string]any) {
	if l == nil {
		return
	}
	entry := make(map[string]any, len(fields)+5)
	for k, v := range fields {
		entry[k] = v
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	entry["seq"] = l.seq
	entry["time"] = l.now().UTC().Format(time.RFC3339Nano)
	entry["event"] = event
	entry["prev"] = l.prev
	body, err := json.Marshal(entry) // map keys are sorted: a canonical form to hash
	if err != nil {
		l.fail(err)
		return
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	// Append "hash" as the last key of the same object.
	line := append(body[:len(body)-1], `,"hash":"`+hash+"\"}\n"...)
	if l.f != nil && l.cfg.MaxBytes > 0 && l.size+int64(len(line)) > l.cfg.MaxBytes && l.size > 0 {
		if err := l.rotate(); err != nil {
			l.fail(err)
		}
	}
	n, err := l.w.Write(line)
	l.size += int64(n)
	if err != nil {
		l.fail(err)
		return
	}
	l.prev = hash
}

func (l *Logger) fail(err error) {
	if l.OnError != nil {
		l.OnError(err)
	}
}

func (l *Logger) rotate() error {
	l.f.Close()
	keep := max(l.cfg.MaxFiles, 1)
	os.Remove(l.cfg.Path + "." + strconv.Itoa(keep))
	for i := keep - 1; i >= 1; i-- {
		os.Rename(l.cfg.Path+"."+strconv.Itoa(i), l.cfg.Path+"."+strconv.Itoa(i+1))
	}
	if err := os.Rename(l.cfg.Path, l.cfg.Path+".1"); err != nil {
		return err
	}
	return l.openFile()
}

// Close flushes and closes the file.
func (l *Logger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Verify checks the hash chain of the files given oldest first (e.g.
// audit.jsonl.3 audit.jsonl.2 audit.jsonl.1 audit.jsonl). It returns the
// number of entries checked, or the first break.
func Verify(paths ...string) (int, error) {
	prev, seq, n := "", uint64(0), 0
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return n, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		line := 0
		for sc.Scan() {
			line++
			raw := sc.Bytes()
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}
			var e map[string]any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber() // re-encode numbers exactly as written
			if err := dec.Decode(&e); err != nil {
				f.Close()
				return n, fmt.Errorf("%s:%d: not JSON: %v", p, line, err)
			}
			hash, _ := e["hash"].(string)
			delete(e, "hash")
			body, _ := json.Marshal(e)
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != hash {
				f.Close()
				return n, fmt.Errorf("%s:%d: entry was modified (hash mismatch)", p, line)
			}
			gotPrev, _ := e["prev"].(string)
			num, _ := e["seq"].(json.Number)
			gotSeq, _ := strconv.ParseUint(string(num), 10, 64)
			if prev != "" && (gotPrev != prev || gotSeq != seq+1) {
				f.Close()
				return n, fmt.Errorf("%s:%d: chain broken (an entry before it was removed, added or reordered)", p, line)
			}
			prev, seq = hash, gotSeq
			n++
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return n, fmt.Errorf("%s: %w", p, err)
		}
	}
	return n, nil
}
