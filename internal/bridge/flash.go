package bridge

import (
	"log/slog"

	"github.com/trick77/relumetv/internal/translate"
)

// TurnOffControlled turns the TV-controlled Ambilight bulbs off. It is used both when
// relumeTV restarts/shuts down (the TV's REST control session drops, so those lights
// would otherwise stay frozen on their last Ambilight color until the TV reconnects)
// and when the TV goes idle (it just stops sending its REST writes — there is no off
// signal). targetUUIDs are the Hue Bridge Pro light UUIDs the TV is currently driving
// (ControlledSet.Current); ONLY those are touched, never the rest of the home. With no
// target UUIDs (the Ambilight set is not known yet — e.g. a first start before the TV
// has driven any light) it is a no-op rather than turning off the whole home. It uses
// its own client and runs synchronously. reason labels the warnings for the caller.
func TurnOffControlled(client proClient, log *slog.Logger, reason string, targetUUIDs []string) {
	if len(targetUUIDs) == 0 {
		if log != nil {
			log.Info(reason + ": no controlled Ambilight lights known yet — skipping (not touching other lights)")
		}
		return
	}

	off := translate.StateV1ToV2(map[string]any{"on": false})
	for _, uuid := range targetUUIDs {
		if err := client.SetLight(uuid, off); err != nil && log != nil {
			log.Warn(reason+": turning light off", "uuid", uuid, "err", err)
		}
	}
}
