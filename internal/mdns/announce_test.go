package mdns

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/trick77/relumetv/internal/config"
)

func TestServiceSpec_matchesHueBridgeAnnouncement(t *testing.T) {
	// Given
	a := New(config.Identity{Serial: "2c4d54ea2832"}, "192.0.2.10", 80, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// When
	spec := a.serviceSpec()

	// Then
	if spec.instance != "Philips Hue - EA2832" {
		t.Errorf("instance = %q", spec.instance)
	}
	if spec.service != "_hue._tcp" {
		t.Errorf("service = %q", spec.service)
	}
	if spec.domain != "local." {
		t.Errorf("domain = %q", spec.domain)
	}
	if spec.host != "2c4d54ea2832" {
		t.Errorf("host = %q", spec.host)
	}
	if !reflect.DeepEqual(spec.txt, []string{"bridgeid=2C4D54FFFEEA2832", "modelid=BSB002"}) {
		t.Errorf("txt = %#v", spec.txt)
	}
}
