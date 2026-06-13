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
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832::urn:schemas-upnp-org:device:basic:1\r\n",
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

func TestSearchResponses_withHassProfileUsesHomeAssistantServerHeader(t *testing.T) {
	// Given
	r := testResponder()
	r.IdentityProfile = "hass"

	// When
	msgs := r.searchResponses()

	// Then
	for _, msg := range msgs {
		if !strings.Contains(msg, "SERVER: Hue/1.0 UPnP/1.0 IpBridge/1.48.0\r\n") {
			t.Errorf("search response missing hass server header:\n%s", msg)
		}
	}
}

func TestSearchResponses_withAmbilightProfileUsesAmbilightServerHeader(t *testing.T) {
	// Given
	r := testResponder()
	r.IdentityProfile = "ambilight"

	// When
	msgs := r.searchResponses()

	// Then
	for _, msg := range msgs {
		if !strings.Contains(msg, "SERVER: Linux/3.14.0 UPnP/1.0 IpBridge/1.67.0\r\n") {
			t.Errorf("search response missing ambilight server header:\n%s", msg)
		}
	}
	joined := strings.Join(msgs, "\n---\n")
	for _, want := range []string{
		"ST: uuid:2f402f80-da50-11e1-9b23-2c4d54fffeea2832\r\n",
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54fffeea2832::upnp:rootdevice\r\n",
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54fffeea2832\r\n",
		"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54fffeea2832::urn:schemas-upnp-org:device:basic:1\r\n",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ambilight search responses missing %q:\n%s", want, joined)
		}
	}
}

func TestSearchResponses_withMediaServerAliasIncludesMediaServerST(t *testing.T) {
	// Given
	r := testResponder()
	r.MediaServerAlias = true

	// When
	msgs := r.searchResponses()

	// Then
	if len(msgs) != 4 {
		t.Fatalf("search response count = %d, expected 4", len(msgs))
	}
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg, "ST: urn:schemas-upnp-org:device:MediaServer:1\r\n") {
			found = true
			for _, want := range []string{
				"CACHE-CONTROL: max-age=1\r\n",
				"LOCATION: http://192.0.2.10:80/description.xml?relume=ms1\r\n",
				"hue-bridgeid: 2C4D54FFFEEA2832\r\n",
				"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832::urn:schemas-upnp-org:device:MediaServer:1\r\n",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("media server search response missing %q:\n%s", want, msg)
				}
			}
		}
	}
	if !found {
		t.Fatalf("search responses do not include MediaServer ST:\n%s", strings.Join(msgs, "\n---\n"))
	}
}

func TestNotifyMessages_withMediaServerAliasIncludesMediaServerAnnouncement(t *testing.T) {
	// Given
	r := testResponder()
	r.MediaServerAlias = true

	// When
	msgs := r.notifyMessages()

	// Then
	if len(msgs) != 4 {
		t.Fatalf("notify message count = %d, expected 4", len(msgs))
	}
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg, "NT: urn:schemas-upnp-org:device:MediaServer:1\r\n") {
			found = true
			for _, want := range []string{
				"NOTIFY * HTTP/1.1\r\n",
				"NTS: ssdp:alive\r\n",
				"CACHE-CONTROL: max-age=1\r\n",
				"LOCATION: http://192.0.2.10:80/description.xml?relume=ms1\r\n",
				"hue-bridgeid: 2C4D54FFFEEA2832\r\n",
				"USN: uuid:2f402f80-da50-11e1-9b23-2c4d54ea2832::urn:schemas-upnp-org:device:MediaServer:1\r\n",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("media server notify missing %q:\n%s", want, msg)
				}
			}
		}
	}
	if !found {
		t.Fatalf("notify messages do not include MediaServer NT:\n%s", strings.Join(msgs, "\n---\n"))
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
