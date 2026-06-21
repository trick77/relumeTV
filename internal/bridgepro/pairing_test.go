package bridgepro

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/trick77/relume-tv/internal/config"
)

// newTestPairer returns a Pairer whose pair attempt succeeds only on/after
// successOn (0-based) and whose capture is a no-op, so no network is touched.
func newTestPairer(successOn int, attempts *int) *Pairer {
	return &Pairer{
		DeviceType: "relume-tv#test",
		Interval:   time.Millisecond,
		capture:    func(*config.BridgePro) {},
		pair: func(*config.BridgePro, string) (*PairResult, error) {
			n := *attempts
			*attempts++
			if n >= successOn {
				return &PairResult{AppKey: "APP", ClientKey: "CLIENT"}, nil
			}
			return nil, errors.New("link button not pressed")
		},
	}
}

func TestPairer_succeedsAfterLinkButtonPressed(t *testing.T) {
	// Given: the button is "pressed" on the third attempt
	attempts := 0
	p := newTestPairer(2, &attempts)
	var seen []int

	// When
	pro, err := p.WaitForLinkButton(context.Background(), &config.BridgePro{Host: "h"}, time.Time{},
		func(a int) { seen = append(seen, a) })

	// Then: keys are filled in and onAttempt fired for the two failures
	if err != nil {
		t.Fatalf("WaitForLinkButton: %v", err)
	}
	if pro.AppKey != "APP" || pro.ClientKey != "CLIENT" {
		t.Fatalf("keys not set: %+v", pro)
	}
	if len(seen) != 2 || seen[0] != 0 || seen[1] != 1 {
		t.Errorf("onAttempt indices = %v, want [0 1]", seen)
	}
}

func TestPairer_deadlineExpires(t *testing.T) {
	// Given: the button is never pressed and a deadline already in the past
	attempts := 0
	p := newTestPairer(1_000_000, &attempts)

	// When / Then: returns an error rather than looping forever
	_, err := p.WaitForLinkButton(context.Background(), &config.BridgePro{Host: "h"},
		time.Now().Add(-time.Second), nil)
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
}

func TestPairer_contextCancelStops(t *testing.T) {
	// Given: the button is never pressed
	attempts := 0
	p := newTestPairer(1_000_000, &attempts)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	// When / Then: the first failed attempt's select observes the cancellation
	_, err := p.WaitForLinkButton(ctx, &config.BridgePro{Host: "h"}, time.Time{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
