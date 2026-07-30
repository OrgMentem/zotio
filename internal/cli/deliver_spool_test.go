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

// A file sink spools to a unique tmp file beside the target. The rename costs
// no second full copy and stays on the target filesystem.
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
	if filepath.Dir(spool.path) != filepath.Dir(target) {
		t.Errorf("spool path = %q, want a sibling of %q", spool.path, target)
	}
	if !strings.HasPrefix(filepath.Base(spool.path), "."+filepath.Base(target)+"-") {
		t.Errorf("spool path = %q, want a target-specific unique tmp name", spool.path)
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
	if _, err := os.Stat(spool.path); !os.IsNotExist(err) {
		t.Error("tmp file survived delivery")
	}
	spool.cleanup()
	spool.cleanup()
	if _, err := os.Stat(target); err != nil {
		t.Errorf("cleanup after commit removed delivered file: %v", err)
	}
}

// The webhook body is streamed off disk with an explicit length. Without the
// length net/http would fall back to chunked encoding, which webhook receivers
// commonly reject. GetBody lets net/http use the seekable spool without
// retaining a second copy in memory.
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

func TestDeliverFileSpoolsUseIndependentTempsForTheSameTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.json")
	first, err := newDeliverSpool(DeliverSink{Scheme: "file", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.cleanup)
	second, err := newDeliverSpool(DeliverSink{Scheme: "file", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.cleanup)

	if first.path == second.path {
		t.Fatalf("spool paths match: %q", first.path)
	}
	firstOutput := "first run begins\nfirst run ends\n"
	secondOutput := "second run begins\nsecond run ends\n"
	for _, write := range []struct {
		spool *deliverSpool
		text  string
	}{
		{first, "first run begins\n"},
		{second, "second run begins\n"},
		{first, "first run ends\n"},
		{second, "second run ends\n"},
	} {
		if _, err := io.WriteString(write.spool, write.text); err != nil {
			t.Fatal(err)
		}
	}

	if err := first.commitFile(); err != nil {
		t.Fatalf("commit first spool: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != firstOutput {
		t.Fatalf("first committed output = %q, want %q", got, firstOutput)
	}

	if err := second.commitFile(); err != nil {
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != firstOutput {
			t.Fatalf("failed second commit changed target to %q, want %q", got, firstOutput)
		}
		return
	}
	got, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != secondOutput {
		t.Errorf("second committed output = %q, want %q", got, secondOutput)
	}
}

func TestDeliverSpoolZeroByteWriteFailureWarns(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.json")
	spool, err := newDeliverSpool(DeliverSink{Scheme: "file", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(spool.cleanup)
	if err := spool.file.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := io.WriteString(spool, "stdout must continue"); err != nil || n != len("stdout must continue") {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len("stdout must continue"))
	}
	if spool.Len() != 0 {
		t.Fatalf("captured length = %d, want zero after failed first write", spool.Len())
	}

	oldStderr := os.Stderr
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = stderrW
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = stderrR.Close()
		_ = stderrW.Close()
	})

	deliverCapturedOutput(nil, context.Background(), DeliverSink{Scheme: "file", Target: target}, spool, false)
	if err := stderrW.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stderr = oldStderr
	stderr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stderr), "warning: deliver to file:"+target+" failed:") {
		t.Fatalf("stderr = %q, want delivery failure warning", stderr)
	}
}

func TestExecuteDeliverFileCommitsBeforeDeferredCleanup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.json")

	if recovered := executeForDeliverSpoolTest(t,
		"--deliver", "file:"+target,
		"which", "citation",
	); recovered != nil {
		t.Fatalf("Execute panicked: %v", recovered)
	}

	delivered, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("delivered file: %v", err)
	}
	if len(delivered) == 0 {
		t.Fatal("delivered file is empty")
	}
	assertNoDeliverTemp(t, target)
}

func TestExecuteDeliverFileCleansUpSpoolAfterPanic(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.json")
	original, hadOriginal := capabilityOverrides["which"]
	capabilityOverrides["which"] = capabilityEntry{Requires: []string{"test-panic"}}
	t.Cleanup(func() {
		if hadOriginal {
			capabilityOverrides["which"] = original
			return
		}
		delete(capabilityOverrides, "which")
	})

	if recovered := executeForDeliverSpoolTest(t,
		"--deliver", "file:"+target,
		"which", "citation",
	); recovered == nil {
		t.Fatal("Execute did not panic")
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target exists after panicking command: %v", err)
	}
	assertNoDeliverTemp(t, target)
}

func executeForDeliverSpoolTest(t *testing.T, args ...string) (recovered any) {
	t.Helper()

	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	oldArgs, oldStdout, oldStderr := os.Args, os.Stdout, os.Stderr
	os.Args = append([]string{"zotio"}, args...)
	os.Stdout, os.Stderr = stdout, stderr
	t.Cleanup(func() {
		os.Args, os.Stdout, os.Stderr = oldArgs, oldStdout, oldStderr
		_ = stdout.Close()
		_ = stderr.Close()
	})

	defer func() {
		recovered = recover()
	}()
	_ = Execute()
	return nil
}

func assertNoDeliverTemp(t *testing.T, target string) {
	t.Helper()

	temps, err := filepath.Glob(filepath.Join(
		filepath.Dir(target),
		"."+filepath.Base(target)+"-*.tmp",
	))
	if err != nil {
		t.Fatalf("glob deliver temps: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("leftover deliver temps: %q", temps)
	}
}
