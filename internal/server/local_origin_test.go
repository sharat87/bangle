package server

import "testing"

// In a container Bangle listens on every interface, but harnesses on the
// host reach it on the host's loopback: the UI must tell them 127.0.0.1,
// never 0.0.0.0.
func TestLocalOrigin(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:7060": "http://127.0.0.1:7060",
		"localhost:7060": "http://localhost:7060",
		"0.0.0.0:7060":   "http://127.0.0.1:7060",
		"[::]:7060":      "http://127.0.0.1:7060",
		":7060":          "http://127.0.0.1:7060",
	} {
		if got := (&Server{listenAddr: listen}).localOrigin(); got != want {
			t.Errorf("localOrigin(%q) = %q, want %q", listen, got, want)
		}
	}
}
