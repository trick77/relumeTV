package bridge

import (
	"log/slog"
	"time"

	"github.com/trick77/relume/internal/translate"
)

// flashRedXY is Hue's red primary in CIE xy.
var flashRedXY = []any{0.675, 0.322}

// flashCount is how many red blinks the restart indicator shows. The durations are
// package vars so tests can shrink them.
const flashCount = 2

var (
	flashOnDur  = 180 * time.Millisecond
	flashOffDur = 160 * time.Millisecond
)

// FlashRestart blinks all Bridge Pro lights red flashCount times and leaves them
// off — a visible "relume restarted" indicator. A relume restart drops the TV's
// REST control session, so the lights would otherwise stay frozen on their last
// Ambilight color until the TV reconnects. It uses its own client and a direct,
// deliberate sequence (not the coalescing control path) and runs synchronously
// (~0.7s). On an unreachable Bridge Pro the initial read fails and it is a no-op.
func FlashRestart(client proClient, log *slog.Logger) {
	lights, err := client.Lights()
	if err != nil {
		if log != nil {
			log.Warn("restart flash: reading lights", "err", err)
		}
		return
	}
	if len(lights) == 0 {
		return
	}

	red := translate.StateV1ToV2(map[string]any{"on": true, "xy": flashRedXY, "bri": 254})
	off := translate.StateV1ToV2(map[string]any{"on": false})
	set := func(body map[string]any) {
		for _, l := range lights {
			if serr := client.SetLight(l.ID, body); serr != nil && log != nil {
				log.Warn("restart flash: setting light", "id", l.ID, "err", serr)
			}
		}
	}

	for i := 0; i < flashCount; i++ {
		set(red)
		time.Sleep(flashOnDur)
		set(off)
		time.Sleep(flashOffDur)
	}
}
