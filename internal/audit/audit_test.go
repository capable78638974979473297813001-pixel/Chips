package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainSurvivesRotationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(Config{Path: path, MaxBytes: 600, MaxFiles: 5})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		l.Log("request", map[string]any{"client": "team-x", "n": i, "cost_usd": 0.000224, "path": "/v1/<chat>&"})
	}
	l.Close()
	// A restart continues the same chain.
	l, err = Open(Config{Path: path, MaxBytes: 600, MaxFiles: 5})
	if err != nil {
		t.Fatal(err)
	}
	l.Log("admin", map[string]any{"action": "reload"})
	l.Close()
	// Restart right after a rotation, with the new file still empty.
	os.Rename(path, path+".1.tmp")
	os.Remove(path + ".5")
	for i := 4; i >= 1; i-- {
		os.Rename(path+"."+string(rune('0'+i)), path+"."+string(rune('0'+i+1)))
	}
	os.Rename(path+".1.tmp", path+".1")
	os.WriteFile(path, nil, 0o600)
	if l, err = Open(Config{Path: path, MaxBytes: 600, MaxFiles: 5}); err != nil {
		t.Fatal(err)
	}
	l.Log("admin", map[string]any{"action": "drain"})
	l.Close()

	files := []string{}
	for i := 5; i >= 1; i-- {
		if _, err := os.Stat(path + "." + string(rune('0'+i))); err == nil {
			files = append(files, path+"."+string(rune('0'+i)))
		}
	}
	if len(files) == 0 {
		t.Fatal("no rotation happened")
	}
	files = append(files, path)
	n, err := Verify(files...)
	if err != nil || n < 6 {
		t.Fatalf("verified %d entries, err %v", n, err)
	}
}

func TestDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"a", "b", "c"} {
		l.Log("request", map[string]any{"client": c, "tokens": 10})
	}
	l.Close()
	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(strings.TrimSpace(string(raw)), "\n")

	edited := strings.Replace(string(raw), `"tokens":10`, `"tokens":1`, 1)
	os.WriteFile(path, []byte(edited), 0o600)
	if _, err := Verify(path); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Errorf("edit: %v", err)
	}
	os.WriteFile(path, []byte(lines[0]+lines[2]), 0o600) // middle line deleted
	if _, err := Verify(path); err == nil || !strings.Contains(err.Error(), "chain broken") {
		t.Errorf("deletion: %v", err)
	}
	// An appended line that isn't ours stops the logger from reopening.
	os.WriteFile(path, []byte(string(raw)+"garbage\n"), 0o600)
	if _, err := Open(Config{Path: path}); err == nil {
		t.Error("opened a log whose last line is not an entry")
	}
}

func TestNilLoggerDiscards(t *testing.T) {
	var l *Logger
	l.Log("x", nil)
	if l.Close() != nil {
		t.Error("close")
	}
}
