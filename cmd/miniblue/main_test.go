package main

import "testing"

func TestListenAddressDefaultsToLoopback(t *testing.T) {
	t.Setenv("LISTEN_HOST", "")

	if got := listenAddress(listenHost(), "4566"); got != "127.0.0.1:4566" {
		t.Fatalf("listenAddress() = %q, want loopback default", got)
	}
}

func TestListenAddressSupportsIPv6(t *testing.T) {
	t.Setenv("LISTEN_HOST", "::1")

	if got := listenAddress(listenHost(), "4566"); got != "[::1]:4566" {
		t.Fatalf("listenAddress() = %q, want IPv6 loopback address", got)
	}
}

func TestIsLoopbackListenHost(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopbackListenHost(host) {
			t.Errorf("isLoopbackListenHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.0.2.1"} {
		if isLoopbackListenHost(host) {
			t.Errorf("isLoopbackListenHost(%q) = true, want false", host)
		}
	}
}
