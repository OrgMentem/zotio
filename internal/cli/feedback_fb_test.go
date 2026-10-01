// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// fbFeedbackReceiver is a TLS feedback endpoint that answers every POST with
// status and records each request body.
type fbFeedbackReceiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func (r *fbFeedbackReceiver) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

// fbNewFeedbackReceiver starts the receiver, lets postFeedback reach it on
// loopback, and trusts its certificate through the default transport that
// externalFetchHTTPClient clones.
func fbNewFeedbackReceiver(t *testing.T, status int) *fbFeedbackReceiver {
	t.Helper()
	recv := &fbFeedbackReceiver{}
	recv.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		recv.mu.Lock()
		recv.bodies = append(recv.bodies, string(data))
		recv.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(recv.srv.Close)
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })
	oldTransport := http.DefaultTransport
	http.DefaultTransport = recv.srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	return recv
}

func fbRunFeedback(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd(&rootFlags{})
	root.SilenceErrors, root.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"feedback"}, args...))
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func fbExitCode(err error) int {
	if err == nil {
		return 0
	}
	return ExitCode(err)
}

func fbReadLedger(t *testing.T) string {
	t.Helper()
	path, err := feedbackFilePath()
	if err != nil {
		t.Fatalf("feedbackFilePath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read feedback ledger: %v", err)
	}
	return string(data)
}

// An explicit --send is a required delivery: when no POST reaches the
// receiver the command keeps the local entry but exits non-zero. The ambient
// auto-send stays best-effort.
func TestFbFeedbackSendExitsNonZeroWhenNotSent(t *testing.T) {
	tests := []struct {
		name         string
		status       int // 0: no endpoint configured
		autoSend     bool
		send         bool
		wantCode     int
		wantSent     bool
		wantWarnLine bool
	}{
		{name: "send without endpoint", send: true, wantCode: 10},
		{name: "send rejected by receiver", status: http.StatusInternalServerError, send: true, wantCode: 5},
		{name: "send delivered", status: http.StatusOK, send: true, wantCode: 0, wantSent: true},
		{name: "auto send rejected stays best-effort", status: http.StatusInternalServerError, autoSend: true, wantCode: 0, wantWarnLine: true},
		{name: "auto send without endpoint stays local", autoSend: true, wantCode: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useWriterLockTestHome(t)
			t.Setenv("ZOTERO_FEEDBACK_ENDPOINT", "")
			t.Setenv("ZOTERO_FEEDBACK_AUTO_SEND", "")
			if tt.autoSend {
				t.Setenv("ZOTERO_FEEDBACK_AUTO_SEND", "true")
			}
			if tt.status != 0 {
				recv := fbNewFeedbackReceiver(t, tt.status)
				t.Setenv("ZOTERO_FEEDBACK_ENDPOINT", recv.srv.URL+"/feedback")
			}
			args := []string{"--json"}
			if tt.send {
				args = append(args, "--send")
			}
			stdout, stderr, err := fbRunFeedback(t, append(args, "send me")...)
			if code := fbExitCode(err); code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; err = %v", code, tt.wantCode, err)
			}
			if !strings.Contains(fbReadLedger(t), "send me") {
				t.Fatal("local feedback entry missing; a failed send must keep the local record")
			}
			var result struct {
				Recorded bool `json:"recorded"`
				Upstream struct {
					Sent bool `json:"sent"`
				} `json:"upstream"`
			}
			if jerr := json.Unmarshal([]byte(stdout), &result); jerr != nil {
				t.Fatalf("decode result %q: %v", stdout, jerr)
			}
			if !result.Recorded || result.Upstream.Sent != tt.wantSent {
				t.Fatalf("result = %+v, want recorded and sent=%v", result, tt.wantSent)
			}
			if got := strings.Contains(stderr, "warning:"); got != tt.wantWarnLine {
				t.Fatalf("stderr warning present = %v, want %v; stderr = %q", got, tt.wantWarnLine, stderr)
			}
		})
	}
}

// A feedback receiver's credential lives in its URL path or query. Neither the
// success result nor any failure message may echo it.
func TestFbFeedbackNeverRevealsEndpointCredential(t *testing.T) {
	const secretPath = "/hooks/fbPathSecret0123456789"
	const secretQuery = "token=fbQuerySecret0123456789"
	tests := []struct {
		name     string
		endpoint func(t *testing.T) string
		wantCode int
	}{
		{
			name: "delivered",
			endpoint: func(t *testing.T) string {
				return fbNewFeedbackReceiver(t, http.StatusOK).srv.URL + secretPath + "?" + secretQuery
			},
		},
		{
			name: "transport failure",
			endpoint: func(t *testing.T) string {
				recv := fbNewFeedbackReceiver(t, http.StatusOK)
				endpoint := recv.srv.URL + secretPath + "?" + secretQuery
				recv.srv.Close() // the dial now fails with a *url.Error carrying the request URL
				return endpoint
			},
			wantCode: 5,
		},
		{
			name: "unparseable endpoint",
			endpoint: func(t *testing.T) string {
				return "https://feedback.example.test" + secretPath + "%zz?" + secretQuery
			},
			wantCode: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useWriterLockTestHome(t)
			t.Setenv("ZOTERO_FEEDBACK_AUTO_SEND", "")
			t.Setenv("ZOTERO_FEEDBACK_ENDPOINT", tt.endpoint(t))
			for _, args := range [][]string{{"--json", "--send", "hi"}, {"--send", "hi"}} {
				stdout, stderr, err := fbRunFeedback(t, args...)
				if code := fbExitCode(err); code != tt.wantCode {
					t.Fatalf("%v exit = %d, want %d; err = %v", args, code, tt.wantCode, err)
				}
				errText := ""
				if err != nil {
					errText = err.Error()
				}
				for _, secret := range []string{"fbPathSecret", "fbQuerySecret"} {
					for stream, text := range map[string]string{"stdout": stdout, "stderr": stderr, "error": errText} {
						if strings.Contains(text, secret) {
							t.Fatalf("%v %s reveals endpoint credential %q: %q", args, stream, secret, text)
						}
					}
				}
			}
		})
	}
}

// AGENT_ID belongs to whatever harness runs zotio. It must not reach the
// ledger or the upstream POST unless the operator passes --agent-id.
func TestFbFeedbackSendsAgentIDOnlyWhenExplicit(t *testing.T) {
	useWriterLockTestHome(t)
	t.Setenv("ZOTERO_FEEDBACK_AUTO_SEND", "")
	t.Setenv("AGENT_ID", "fb-ambient-agent")
	recv := fbNewFeedbackReceiver(t, http.StatusOK)
	t.Setenv("ZOTERO_FEEDBACK_ENDPOINT", recv.srv.URL+"/feedback")

	if _, _, err := fbRunFeedback(t, "--send", "first"); err != nil {
		t.Fatalf("feedback --send: %v", err)
	}
	if _, _, err := fbRunFeedback(t, "--send", "--agent-id", "fb-chosen-agent", "second"); err != nil {
		t.Fatalf("feedback --send --agent-id: %v", err)
	}

	bodies := recv.received()
	if len(bodies) != 2 {
		t.Fatalf("upstream POSTs = %d, want 2", len(bodies))
	}
	ledger := fbReadLedger(t)
	for where, text := range map[string]string{"POST body": bodies[0] + bodies[1], "ledger": ledger} {
		if strings.Contains(text, "fb-ambient-agent") {
			t.Fatalf("%s carries the ambient AGENT_ID: %q", where, text)
		}
	}
	var first, second FeedbackEntry
	if err := json.Unmarshal([]byte(bodies[0]), &first); err != nil {
		t.Fatalf("decode first POST %q: %v", bodies[0], err)
	}
	if err := json.Unmarshal([]byte(bodies[1]), &second); err != nil {
		t.Fatalf("decode second POST %q: %v", bodies[1], err)
	}
	if first.AgentID != "" || strings.Contains(bodies[0], "agent_id") {
		t.Fatalf("first POST = %q, want no agent_id without --agent-id", bodies[0])
	}
	if second.AgentID != "fb-chosen-agent" {
		t.Fatalf("second POST agent_id = %q, want the --agent-id value", second.AgentID)
	}
}
