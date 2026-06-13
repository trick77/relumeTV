package diag

import (
	"net"
	"testing"
)

func TestShouldLogMDNS_keepsDefaultHueOnly(t *testing.T) {
	// Given
	o := NewMDNSObserver("192.0.2.10", nil)

	// Then
	if !o.shouldLogMDNS(net.ParseIP("192.0.2.20"), []string{"_hue._tcp.local"}) {
		t.Fatal("expected hue query to be logged")
	}
	if o.shouldLogMDNS(net.ParseIP("192.0.2.20"), []string{"_googlecast._tcp.local"}) {
		t.Fatal("expected non-hue query to be ignored by default")
	}
}

func TestShouldLogMDNS_logsAllQuestionsFromTVIP(t *testing.T) {
	// Given
	o := NewMDNSObserver("192.0.2.10", nil)
	o.DebugTVIP = "192.0.2.30"

	// Then
	if !o.shouldLogMDNS(net.ParseIP("192.0.2.30"), []string{"_googlecast._tcp.local"}) {
		t.Fatal("expected all questions from the configured TV IP to be logged")
	}
	if o.shouldLogMDNS(net.ParseIP("192.0.2.31"), []string{"_googlecast._tcp.local"}) {
		t.Fatal("expected non-hue query from another host to be ignored")
	}
}
