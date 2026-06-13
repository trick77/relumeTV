package ssdp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/trick77/relume/internal/config"
)

func testResponder() *Responder {
	return New(config.Identity{Serial: "2c4d54ea2832"}, "192.0.2.10", 80, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestNotifyMessages_matchHueBridgeShape(t *testing.T) {
	// Given
	r := testResponder()

	// When
	msgs := r.notifyMessages()

	// Then
	if len(msgs) != 3 {
		t.Fatalf("notify message count = %d, expected 3", len(msgs))
	}
	for _, want := range []string{
		"NT: upnp:rootdevice\r\n",
		"NT: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832\r\n",
		"NT: urn:schemas-upnp-org:device:basic:1\r\n",
	} {
		found := false
		for _, msg := range msgs {
			if strings.Contains(msg, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("notify messages do not contain %q:\n%s", want, strings.Join(msgs, "\n---\n"))
		}
	}
	for _, msg := range msgs {
		for _, want := range []string{
			"NOTIFY * HTTP/1.1\r\n",
			"HOST: 239.255.255.250:1900\r\n",
			"LOCATION: http://192.0.2.10:80/description.xml\r\n",
			"SERVER: Linux/3.14.0 UPnP/1.0 IpBridge/1.20.0\r\n",
			"NTS: ssdp:alive\r\n",
			"hue-bridgeid: 2C4D54FFFEEA2832\r\n",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("notify message missing %q:\n%s", want, msg)
			}
		}
	}
}

func TestRunBurst_sendsImmediatelyAndOnIntervalUntilDuration(t *testing.T) {
	// Given
	ctx := context.Background()
	count := 0

	// When
	runBurst(ctx, 10*time.Millisecond, 35*time.Millisecond, func() {
		count++
	})

	// Then: one immediate send, then roughly at 10ms/20ms/30ms.
	if count < 4 {
		t.Fatalf("burst count = %d, expected at least 4", count)
	}
	if count > 5 {
		t.Fatalf("burst count = %d, expected no more than 5", count)
	}
}
