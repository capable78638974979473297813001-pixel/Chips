package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/capable78638974979473297813001-pixel/chips/internal/limits"
)

const (
	cacheFile = "response-cache.gob"
	usageFile = "usage.json"
)

// SaveState writes the response cache and usage counters to dir. Each file
// is written to a temporary name and renamed, so a crash mid-write never
// leaves a half-written file behind.
func (s *Server) SaveState(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var errs []error
	if c := s.state.Load().cache; c != nil {
		errs = append(errs, writeAtomic(filepath.Join(dir, cacheFile), c.Save))
	}
	errs = append(errs, writeAtomic(filepath.Join(dir, usageFile), func(w io.Writer) error {
		return json.NewEncoder(w).Encode(s.limits.Snapshot())
	}))
	return errors.Join(errs...)
}

// LoadState restores what SaveState wrote. Missing files are not an error.
func (s *Server) LoadState(dir string) (cached int, err error) {
	if c := s.state.Load().cache; c != nil {
		if f, err := os.Open(filepath.Join(dir, cacheFile)); err == nil {
			cached, err = c.Load(f)
			f.Close()
			if err != nil {
				return 0, fmt.Errorf("restore response cache: %w", err)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, usageFile)); err == nil {
		var saved []limits.Usage
		if err := json.Unmarshal(b, &saved); err != nil {
			return cached, fmt.Errorf("restore usage: %w", err)
		}
		s.limits.Restore(saved)
	}
	return cached, nil
}

func writeAtomic(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
