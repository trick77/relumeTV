package bridge

import (
	"log/slog"
	"time"

	"github.com/trick77/relume/internal/translate"
)

// flashRedXY is Hue's red primary in CIE xy.
var flashRedXY = []any{0.675, 0.322}

// flashGreenXY is Hue's green primary in CIE xy.
var flashGreenXY = []any{0.217, 0.722}

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
// Ambilight color until the TV reconnects. See flashColor for the mechanics.
func FlashRestart(client proClient, log *slog.Logger) {
	flashColor(client, log, flashRedXY, "restart flash")
}

// FlashIdle blinks all Bridge Pro lights green flashCount times and leaves them
// off — the idle-off indicator. When the TV is switched off it simply stops
// sending its REST light-state writes (there is no off signal), so the lights
// would otherwise stay frozen on their last Ambilight color. The idle monitor
// (see cmd/relume) calls this once the writes have gone silent for the timeout.
func FlashIdle(client proClient, log *slog.Logger) {
	flashColor(client, log, flashGreenXY, "idle flash")
}

// flashColor blinks all Bridge Pro lights with the given CIE-xy color flashCount
// times and leaves them off. It uses its own client and a direct, deliberate
// sequence (not the coalescing control path) and runs synchronously (~0.7s). On
// an unreachable Bridge Pro the initial read fails and it is a no-op. logPrefix
// labels the warnings for the calling indicator.
func flashColor(client proClient, log *slog.Logger, colorXY []any, logPrefix string) {
	lights, err := client.Lights()
	if err != nil {
		if log != nil {
			log.Warn(logPrefix+": reading lights", "err", err)
		}
		return
	}
	if len(lights) == 0 {
		return
	}

	on := translate.StateV1ToV2(map[string]any{"on": true, "xy": colorXY, "bri": 254})
	off := translate.StateV1ToV2(map[string]any{"on": false})
	set := func(body map[string]any) {
		for _, l := range lights {
			if serr := client.SetLight(l.ID, body); serr != nil && log != nil {
				log.Warn(logPrefix+": setting light", "id", l.ID, "err", serr)
			}
		}
	}

	for i := 0; i < flashCount; i++ {
		set(on)
		time.Sleep(flashOnDur)
		set(off)
		time.Sleep(flashOffDur)
	}
}
