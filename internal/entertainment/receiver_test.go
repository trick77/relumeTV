package entertainment

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/trick77/relume/internal/huestream"
)

// TestReceiver_decodesStreamOverDTLS drives the receiver end-to-end: a pion DTLS
// client authenticates with the PSK and streams a HueStream v1 frame, and the
// receiver decrypts + decodes it (observed via OnFrame).
func TestReceiver_decodesStreamOverDTLS(t *testing.T) {
	const identity = "tvuser"
	psk := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	const port = 32100

	frames := make(chan *huestream.Frame, 4)
	r := &Receiver{
		bindIP: "127.0.0.1",
		Port:   port,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		lookup: func(id string) ([]byte, bool) {
			if id == identity {
				return psk, true
			}
			return nil, false
		},
		OnFrame: func(_ string, f *huestream.Frame) { frames <- f },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	// Wait for the listener to come up, then connect a DTLS-PSK client.
	var conn *dtls.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := dtls.Dial("udp",
			&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port},
			&dtls.Config{
				PSK:                  func([]byte) ([]byte, error) { return psk, nil },
				PSKIdentityHint:      []byte(identity),
				CipherSuites:         []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
				ExtendedMasterSecret: dtls.DisableExtendedMasterSecret,
			})
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dtls dial: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer conn.Close()

	if _, err := conn.Write(v1FrameLightsSixEleven()); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	select {
	case f := <-frames:
		if len(f.Channels) != 2 || f.Channels[0].ID != 6 || f.Channels[1].ID != 11 {
			t.Fatalf("decoded channels = %+v", f.Channels)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a decoded frame")
	}
}

// v1FrameLightsSixEleven builds a minimal HueStream v1 RGB frame for lights 6/11.
func v1FrameLightsSixEleven() []byte {
	b := []byte("HueStream")
	b = append(b, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00)
	b = append(b, 0x00, 0x00, 0x06, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00)
	b = append(b, 0x00, 0x00, 0x0B, 0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00)
	return b
}
