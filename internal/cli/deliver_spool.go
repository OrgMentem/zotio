// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// deliverSpool streams captured command output to a file instead of holding it
// on the heap.
//
// --deliver used to install an unbounded bytes.Buffer beside stdout and then
// hand Deliver a second full copy of the result. Commands whose output scales
// with the library -- export, list, audit, sync -- could exhaust the heap
// producing a payload that streams to disk without trouble, and a failed
// webhook kept the whole buffer live until the command returned.
//
// For a file sink the spool IS the atomic tmp file the rename will promote, so
// delivery costs a rename rather than a copy and cannot cross a filesystem
// boundary. Other sinks spool to the OS temp dir and replay from there, which
// is also what makes a webhook retry cheap: the body is re-read, not re-held.
type deliverSpool struct {
	file   *os.File
	target string // non-empty when the spool is a file sink's tmp file
	n      int64
	// writeErr records the first spool failure. Writes keep reporting success
	// to the MultiWriter regardless: stdout is the primary output and must not
	// be truncated because the spool's disk filled up.
	writeErr error
}

func newDeliverSpool(sink DeliverSink) (*deliverSpool, error) {
	if sink.Scheme == "file" {
		dir := filepath.Dir(sink.Target)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("creating deliver dir: %w", err)
			}
		}
		tmp := sink.Target + ".tmp"
		file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening deliver tmp: %w", err)
		}
		return &deliverSpool{file: file, target: sink.Target}, nil
	}
	file, err := os.CreateTemp("", "zotio-deliver-*")
	if err != nil {
		return nil, fmt.Errorf("creating deliver spool: %w", err)
	}
	return &deliverSpool{file: file}, nil
}

func (s *deliverSpool) Write(p []byte) (int, error) {
	if s == nil || s.file == nil {
		return len(p), nil
	}
	written, err := s.file.Write(p)
	s.n += int64(written)
	if err != nil && s.writeErr == nil {
		s.writeErr = err
	}
	return len(p), nil
}

// Len reports the bytes captured so far, standing in for bytes.Buffer.Len on
// the "did this command produce anything to deliver" check.
func (s *deliverSpool) Len() int64 {
	if s == nil {
		return 0
	}
	return s.n
}

// commitFile promotes the spool to its final path. The tmp+rename keeps an
// agent from observing a half-written file, exactly as the buffered
// implementation did.
func (s *deliverSpool) commitFile() error {
	if s.writeErr != nil {
		return fmt.Errorf("writing deliver tmp: %w", s.writeErr)
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("closing deliver tmp: %w", err)
	}
	if err := os.Rename(s.target+".tmp", s.target); err != nil {
		return fmt.Errorf("replacing deliver file: %w", err)
	}
	return nil
}

// reader rewinds the spool for replay. Returned as an io.ReadSeeker so an HTTP
// body can be re-read on redirect or retry without a second copy in memory.
func (s *deliverSpool) reader() (io.ReadSeeker, error) {
	if s.writeErr != nil {
		return nil, fmt.Errorf("writing deliver spool: %w", s.writeErr)
	}
	if err := s.file.Sync(); err != nil {
		return nil, fmt.Errorf("flushing deliver spool: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewinding deliver spool: %w", err)
	}
	return s.file, nil
}

// cleanup removes whatever the spool left behind. Safe to call twice, and safe
// after a successful commit, where the tmp path no longer exists.
func (s *deliverSpool) cleanup() {
	if s == nil || s.file == nil {
		return
	}
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
	s.file = nil
}
