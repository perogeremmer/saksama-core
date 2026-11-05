package app

import (
	"errors"
	"strings"
	"testing"
)

// The message has to separate the two TLS failures, because only one of them is
// fixed by --tls skip-verify. Telling someone to skip verification when the
// protocol is the problem sends them round the loop twice.
func TestTLSHintSeparatesProtocolFromCertificate(t *testing.T) {
	protocol := errors.New("remote error: tls: handshake failure")
	if got := tlsHint(protocol, "preferred"); !strings.Contains(got, "protocol or cipher list") ||
		strings.Contains(got, "skip-verify accepts it") {
		t.Fatalf("protocol failure hint = %q", got)
	}

	cert := errors.New("x509: certificate signed by unknown authority")
	if got := tlsHint(cert, "preferred"); !strings.Contains(got, "skip-verify") {
		t.Fatalf("certificate failure must mention skip-verify: %q", got)
	}

	for _, c := range []struct {
		name string
		err  error
		mode string
	}{
		{"already disabled", protocol, "false"},
		{"not a TLS error", errors.New("Error 1045: Access denied"), "preferred"},
		{"no error", nil, "preferred"},
	} {
		if got := tlsHint(c.err, c.mode); got != "" {
			t.Errorf("%s: hint should be empty, got %q", c.name, got)
		}
	}
}
