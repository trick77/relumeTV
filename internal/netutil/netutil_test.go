package netutil

import "testing"

func TestInterfaceForIP_InvalidIP(t *testing.T) {
	if _, err := InterfaceForIP("not-an-ip"); err == nil {
		t.Fatal("expected error for invalid IP, got nil")
	}
}

func TestInterfaceForIP_EmptyIP(t *testing.T) {
	if _, err := InterfaceForIP(""); err == nil {
		t.Fatal("expected error for empty IP, got nil")
	}
}
