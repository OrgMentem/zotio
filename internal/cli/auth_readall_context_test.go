// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// A cancelled read of a command-owned reader must release it: the blocked
// reader goroutine is unblocked and joined, not parked until the input closes.
func TestReadAllContextClosesOwnedReaderOnCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := readAllContext(ctx, pr)
		errc <- err
	}()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readAllContext err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readAllContext did not return after cancel")
	}
	if _, err := pw.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after cancel err = %v, want io.ErrClosedPipe (reader closed)", err)
	}
}

// A file-backed reader (the process stdin is one) is interrupted with a read
// deadline rather than closed, and stays usable once the read is joined.
func TestReadAllContextInterruptsFileReaderWithoutClosing(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := readAllContext(ctx, r)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readAllContext err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readAllContext did not return after cancel")
	}
	// Still open and readable: the deadline was cleared.
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatalf("write to pipe after cancel: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read after cancel = %q, %v; want the descriptor left open and readable", buf, err)
	}
}
