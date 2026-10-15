package auth

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Store holds the active token set and swaps it atomically on reload.
type Store struct {
	path    string
	current atomic.Pointer[TokenSet]
	logger  *slog.Logger

	mu      sync.Mutex // serialises reloads
	seen    []byte     // content of the last file read, valid or not
	seenErr error

	// OnReload is called after every reload attempt that saw new content, with
	// the number of active tokens and the error, if the file was rejected.
	OnReload func(tokens int, err error)
}

// NewStore loads path once. A gateway must not start without valid tokens.
func NewStore(path string, logger *slog.Logger) (*Store, error) {
	set, raw, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, seen: raw, logger: logger}
	s.current.Store(set)
	return s, nil
}

// Tokens returns the active token set.
func (s *Store) Tokens() *TokenSet { return s.current.Load() }

// Reload re-reads the file and applies it when its content changed. An
// invalid file is reported once and ignored: the previous token set stays
// active until a valid file appears.
func (s *Store) Reload() (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	set, raw, err := LoadFile(s.path)
	if raw == nil && err != nil {
		raw = []byte("\x00" + err.Error()) // dedupe repeated read errors too
	}
	if bytes.Equal(raw, s.seen) {
		return false, s.seenErr
	}
	s.seen, s.seenErr = raw, err

	if err != nil {
		s.logger.Error("token file rejected, keeping previous configuration", "path", s.path, "err", err)
	} else {
		s.current.Store(set)
		s.logger.Info("token file reloaded", "path", s.path, "tokens", set.Len())
	}
	if s.OnReload != nil {
		n := s.current.Load().Len()
		s.OnReload(n, err)
	}
	return err == nil, err
}

// Watch polls the file every interval until ctx is done. Polling (instead of
// inotify) also follows the symlink swap Kubernetes uses to update mounted
// Secrets.
func (s *Store) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.Reload()
		}
	}
}
