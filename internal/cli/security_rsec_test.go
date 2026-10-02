// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"zotio/internal/connector"
)

func TestRsecMalformedWebhookURLsHideSecrets(t *testing.T) {
	const raw = "https://user:RSEC-PASSWORD@example.com/RSEC-PATH%zz"
	_, deliverErr := ParseDeliverSinkWithContext(context.Background(), "webhook:"+raw)
	_, healthErr := newWatchHealthMonitorWithContext(context.Background(), &rootFlags{}, true, "quick", raw)
	for name, err := range map[string]error{"deliver": deliverErr, "health-webhook": healthErr} {
		if err == nil || !strings.Contains(err.Error(), "not a valid URL") {
			t.Fatalf("%s: want malformed URL refusal, got %v", name, err)
		}
		if strings.Contains(err.Error(), "RSEC-") || strings.Contains(err.Error(), "example.com") {
			t.Fatalf("%s leaked URL: %v", name, err)
		}
	}
}

func TestRsecSetTokenPositionalRefusalHidesSecret(t *testing.T) {
	cmd := newRootCmd(&rootFlags{})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"auth", "set-token", "SECRET123"})
	err := cmd.Execute()
	if ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2; error = %v", ExitCode(err), err)
	}
	// main prints returned errors to stderr; check that exact text as well.
	fmt.Fprintln(&stderr, err)
	if strings.Contains(stderr.String(), "SECRET123") {
		t.Fatalf("stderr leaked positional token: %s", stderr.String())
	}
	if !strings.Contains(err.Error(), "refusing token on command line") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

type rsecFailTransport struct{}

func (rsecFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

func TestRsecDoctorUnreachableAPIHidesCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_BASE_URL", "")
	oldTransport := http.DefaultTransport
	http.DefaultTransport = rsecFailTransport{}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	oldPing := connectorPing
	connectorPing = func(context.Context, *connector.Client) error { return errors.New("disabled") }
	t.Cleanup(func() { connectorPing = oldPing })
	cmd := newDoctorCmd(&rootFlags{
		asJSON:     true,
		configPath: doctorTestConfigFile(t, "http://u:sekret@localhost:12345/api/users/0?token=abc"),
		timeout:    time.Second,
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable:") {
		t.Fatalf("doctor did not report transport failure: %s", out.String())
	}
	for _, secret := range []string{"abc", "sekret"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("doctor leaked %q: %s", secret, out.String())
		}
	}
}
