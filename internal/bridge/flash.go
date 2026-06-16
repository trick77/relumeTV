package bridge

import (
	"log/slog"
	"time"

	"github.com/trick77/relume/internal/translate"
)

// flashGreenXY / flashBlueXY are Hue's green and blue primaries in CIE xy. The
// restart flash is green, the idle flash is blue.
var (
	flashGreenXY = []any{0.217, 0.722}
	flashBlueXY  = []any{0.167, 0.04}
)

// restartFlashCount / idleFlashCount are how many blinks each indicator shows — the
// restart flash (green) and the idle flash (blue) each blink twice; they are
// distinguished by colour. The durations are package vars so tests can shrink them.
const (
	restartFlashCount = 2
	idleFlashCount    = 2
)

var (
	flashOnDur  = 180 * time.Millisecond
	flashOffDur = 160 * time.Millisecond
)

// FlashRestart blinks the TV-controlled Ambilight bulbs green restartFlashCount
// times and leaves them off — a visible "relume restarted" indicator. A relume
// restart drops the TV's REST control session, so those lights would otherwise stay
// frozen on their last Ambilight color until the TV reconnects. targetUUIDs are
// the Bridge Pro light UUIDs the TV is currently driving (ControlledSet.Current);
// ONLY those are touched, never the rest of the home. See flashColor.
func FlashRestart(client proClient, log *slog.Logger, targetUUIDs []string) {
	flashColor(client, log, flashGreenXY, restartFlashCount, "restart flash", targetUUIDs)
}

// FlashIdle blinks the TV-controlled Ambilight bulbs blue idleFlashCount times and
// leaves them off — the idle-off indicator. When the TV is switched off it simply
// stops sending its REST light-state writes (there is no off signal), so those
// lights would otherwise stay frozen on their last Ambilight color. The idle
// monitor (see cmd/relume) calls this once the writes have gone silent for the
// timeout. targetUUIDs scopes it to the Ambilight bulbs only — never the rest of
// the home.
func FlashIdle(client proClient, log *slog.Logger, targetUUIDs []string) {
	flashColor(client, log, flashBlueXY, idleFlashCount, "idle flash", targetUUIDs)
}

// flashColor blinks the given Bridge Pro light UUIDs with the CIE-xy color
// count times and leaves them off. It uses its own client and a direct,
// deliberate sequence (not the coalescing control path) and runs synchronously
// (~0.7s). With no target UUIDs (the Ambilight set is not known yet — e.g. a
// first start before the TV has driven any light) it is a no-op rather than
// touching the whole home. logPrefix labels the warnings for the calling indicator.
func flashColor(client proClient, log *slog.Logger, colorXY []any, count int, logPrefix string, targetUUIDs []string) {
	if len(targetUUIDs) == 0 {
		if log != nil {
			log.Info(logPrefix + ": no controlled Ambilight lights known yet — skipping (not touching other lights)")
		}
		return
	}

	on := translate.StateV1ToV2(map[string]any{"on": true, "xy": colorXY, "bri": 254})
	off := translate.StateV1ToV2(map[string]any{"on": false})
	set := func(body map[string]any) {
		for _, uuid := range targetUUIDs {
			if serr := client.SetLight(uuid, body); serr != nil && log != nil {
				log.Warn(logPrefix+": setting light", "uuid", uuid, "err", serr)
			}
		}
	}

	for i := 0; i < count; i++ {
		set(on)
		time.Sleep(flashOnDur)
		set(off)
		time.Sleep(flashOffDur)
	}
}
