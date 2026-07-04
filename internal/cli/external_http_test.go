package cli

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestPublicDialContextRejectsPrivateIPBeforeDial(t *testing.T) {
	conn, err := publicDialContext(context.Background(), "tcp", "10.0.0.1:443")
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("publicDialContext accepted private IP dial target, want rejection")
	}
	if conn != nil {
		_ = conn.Close()
		t.Fatal("publicDialContext returned a connection for a private IP dial target")
	}
	if !strings.Contains(err.Error(), "local or private") {
		t.Fatalf("publicDialContext private target error = %v, want local/private rejection", err)
	}
}

func TestValidateExternalHTTPURLAllowsNonResolvingStoredLink(t *testing.T) {
	oldResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, errors.New("dns disabled in test")
		},
	}
	t.Cleanup(func() { net.DefaultResolver = oldResolver })

	if err := validateExternalHTTPURL("https://does-not-resolve.example.invalid/resource", false); err != nil {
		t.Fatalf("validateExternalHTTPURL rejected non-resolving stored link: %v", err)
	}
}
