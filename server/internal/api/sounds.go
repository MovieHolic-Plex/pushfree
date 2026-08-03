package api

import "net/http"

// pushoverSounds is the exact 23-entry built-in sound map returned by
// GET /1/sounds.json. Keys and display-name values are copied verbatim from
// the #sounds section of pushover.net/api (fetched 2026-08-03): 16 standard
// tones, 5 long tones, plus "vibrate" and "none". pushfree stores but does not
// synthesize custom sounds (a Pushover feature since 2021-04), so only these
// 23 built-ins are listed. The map is never mutated after init; the handler
// returns it directly and encoding/json emits keys in sorted order, so the
// response body is byte-stable across calls.
var pushoverSounds = map[string]string{
	// 16 standard tones
	"pushover":     "Pushover (default)",
	"bike":         "Bike",
	"bugle":        "Bugle",
	"cashregister": "Cash Register",
	"classical":    "Classical",
	"cosmic":       "Cosmic",
	"falling":      "Falling",
	"gamelan":      "Gamelan",
	"incoming":     "Incoming",
	"intermission": "Intermission",
	"magic":        "Magic",
	"mechanical":   "Mechanical",
	"pianobar":     "Piano Bar",
	"siren":        "Siren",
	"spacealarm":   "Space Alarm",
	"tugboat":      "Tug Boat",
	// 5 long tones
	"alien":      "Alien Alarm (long)",
	"climb":      "Climb (long)",
	"persistent": "Persistent (long)",
	"echo":       "Pushover Echo (long)",
	"updown":     "Up Down (long)",
	// vibration / silent
	"vibrate": "Vibrate Only",
	"none":    "None (silent)",
}

// soundsHandler implements GET /1/sounds.json (plan todo 11). It mirrors
// Pushover's built-in sound catalog: given a valid app token (query param
// token), it returns {"status":1,"sounds":{...23 entries...}}. A missing or
// invalid token yields the canonical {"status":0,"errors":[...]} envelope
// with HTTP 400. The catalog is fixed at compile time; there is no per-user
// customization.
//
// This is a method on *Accounts so it shares ValidateAppToken with the rest
// of the /1/* surface.
func (a *Accounts) soundsHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if _, err := a.ValidateAppToken(r.Context(), token); err != nil {
		writeErrors(w, http.StatusBadRequest, "application token is invalid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": 1,
		"sounds": pushoverSounds,
	})
}
