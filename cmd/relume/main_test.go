package main

import (
	"context"
	"testing"
	"time"
)

func TestParseServeOptions_discoveryDiagnostics(t *testing.T) {
	// When
	opts, err := parseServeOptions([]string{
		"-config", "test.json",
		"-http-port", "8080",
		"-advertise-ip", "192.0.2.10",
		"-debug",
		"-tv-ip", "192.0.2.30",
		"-discovery-burst-duration", "90s",
		"-discovery-burst-interval", "1s",
		"-identity-profile", "ambilight",
		"-description-profile", "ambilight-reference",
		"-ssdp-media-server-alias",
		"-ssdp-media-server-basic-body",
		"-ssdp-descriptor-variants",
	})

	// Then
	if err != nil {
		t.Fatalf("parseServeOptions: %v", err)
	}
	if opts.configPath != "test.json" {
		t.Errorf("configPath = %q", opts.configPath)
	}
	if opts.httpPort != 8080 {
		t.Errorf("httpPort = %d", opts.httpPort)
	}
	if opts.advertiseIP != "192.0.2.10" {
		t.Errorf("advertiseIP = %q", opts.advertiseIP)
	}
	if !opts.debug {
		t.Fatal("debug = false")
	}
	if opts.tvIP != "192.0.2.30" {
		t.Errorf("tvIP = %q", opts.tvIP)
	}
	if opts.discoveryBurstDuration != 90*time.Second {
		t.Errorf("discoveryBurstDuration = %s", opts.discoveryBurstDuration)
	}
	if opts.discoveryBurstInterval != time.Second {
		t.Errorf("discoveryBurstInterval = %s", opts.discoveryBurstInterval)
	}
	if opts.identityProfile != "ambilight" {
		t.Errorf("identityProfile = %q", opts.identityProfile)
	}
	if opts.descriptionProfile != "ambilight-reference" {
		t.Errorf("descriptionProfile = %q", opts.descriptionProfile)
	}
	if !opts.ssdpMediaServerAlias {
		t.Fatal("ssdpMediaServerAlias = false")
	}
	if !opts.ssdpMediaServerBasicBody {
		t.Fatal("ssdpMediaServerBasicBody = false")
	}
	if !opts.ssdpDescriptorVariants {
		t.Fatal("ssdpDescriptorVariants = false")
	}
	if opts.disableSSDP {
		t.Fatal("disableSSDP = true (not requested)")
	}
}

func TestParseServeOptions_disableSSDP(t *testing.T) {
	// When
	opts, err := parseServeOptions([]string{"-disable-ssdp"})

	// Then
	if err != nil {
		t.Fatalf("parseServeOptions: %v", err)
	}
	if !opts.disableSSDP {
		t.Fatal("disableSSDP = false")
	}
}

func TestParseServeOptions_bridgeProAutoPairFlags(t *testing.T) {
	// When
	opts, err := parseServeOptions([]string{"-bridge-ip", "192.0.2.50", "-skip-tls-verify"})

	// Then
	if err != nil {
		t.Fatalf("parseServeOptions: %v", err)
	}
	if opts.bridgeIP != "192.0.2.50" {
		t.Errorf("bridgeIP = %q", opts.bridgeIP)
	}
	if !opts.skipTLS {
		t.Fatal("skipTLS = false")
	}
}

func TestSleepCtx_returnsFalseWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx returned true for a cancelled context")
	}
}

func TestSleepCtx_returnsTrueAfterDelay(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("sleepCtx returned false after a normal delay")
	}
}
