package main

import "testing"

func TestIsLoopbackHTTPAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want bool
	}{
		{name: "ipv4 loopback", addr: "127.0.0.1:7777", want: true},
		{name: "localhost", addr: "localhost:7777", want: true},
		{name: "empty host wildcard", addr: ":7777", want: false},
		{name: "ipv4 wildcard", addr: "0.0.0.0:7777", want: false},
		{name: "ipv6 loopback", addr: "[::1]:7777", want: true},
		{name: "remote ipv4", addr: "1.2.3.4:7777", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackHTTPAddr(tt.addr); got != tt.want {
				t.Fatalf("isLoopbackHTTPAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}
