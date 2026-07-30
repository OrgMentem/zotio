// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file sink spools straight to the tmp file the rename will promote, so
// delivery costs a rename rather than a second full copy -- and, because the
// tmp sits beside the target, it can never land on a different filesystem.
func TestDeliverFileSpoolsBesideTheTargetAndRenamesIntoPlace(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "out.json")
	spool, err := newDeliverSpool(DeliverSink{Scheme: "file", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(spool.cleanup)

	payload := strings.Repeat("payload\n", 4096)
	if _, err := io.WriteString(spool, payload); err != nil {
		t.Fatal(err)
	}
	if spool.file.Name() != target+".tmp" {
		t.Errorf("spool path = %q, want the sibling tmp of %q", spool.file.Name(), target)
	}
	// Nothing is visible at the target until the command succeeds.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("target exists before delivery; a partial file was observable")
	}

	if err := Deliver(context.Background(), DeliverSink{Scheme: "file", Target: target}, spool, false); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Errorf("delivered %d bytes, want %d", len(got), len(payload))
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file survived delivery")
	}
}

// The webhook body is streamed off disk with an explicit length. Without the
// length net/http would fall back to chunked encoding, which webhook receivers
// commonly reject, and GetBody is what lets a redirect or retry replay the
// body without holding a second copy in memory.
func TestDeliverWebhookStreamsTheSpoolWithADeclaredLength(t *testing.T) {
	payload := strings.Repeat("event\n", 8192)
	var gotBody []byte
	var gotLength int64
	var gotEncoding []string
	var gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		gotBody, gotLength, gotEncoding = body, r.ContentLength, r.TransferEncoding
		gotType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	sink := DeliverSink{Scheme: "webhook", Target: srv.URL + "/hook"}
	spool, err := newDeliverSpool(sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(spool.cleanup)
	if _, err := io.WriteString(spool, payload); err != nil {
		t.Fatal(err)
	}

	if err := Deliver(context.Background(), sink, spool, true); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if string(gotBody) != payload {
		t.Errorf("posted %d bytes, want %d", len(gotBody), len(payload))
	}
	if gotLength != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d", gotLength, len(payload))
	}
	if len(gotEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none", gotEncoding)
	}
	if gotType != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson under --compact", gotType)
	}
}

// stdout is the primary output. A spool that cannot write -- full disk, revoked
// permissions -- must not truncate it, so Write always reports success and the
// failure surfaces at delivery instead.
func TestDeliverSpoolWriteFailureDoesNotTruncateStdout(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.json")
	spool, err := newDeliverSpool(DeliverSink{Scheme: "file", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(spool.cleanup)
	// Close underneath the spool so every subsequent write fails.
	if err := spool.file.Close(); err != nil {
		t.Fatal(err)
	}

	n, err := io.WriteString(spool, "output the user still needs to see")
	if err != nil {
		t.Errorf("Write returned %v; a spool failure must not fail the stdout writer", err)
	}
	if n != len("output the user still needs to see") {
		t.Errorf("Write reported %d bytes; a short count would truncate the MultiWriter", n)
	}
	if derr := Deliver(context.Background(), DeliverSink{Scheme: "file", Target: target}, spool, false); derr == nil {
		t.Error("Deliver reported success after the spool failed to write")
	}
}
