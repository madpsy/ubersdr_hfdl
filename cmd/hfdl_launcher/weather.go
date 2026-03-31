package main

// ---------------------------------------------------------------------------
// Weather messages — ACARS label H1 / sublabel WX ring buffer
//
// Weather data in HFDL flows as plain text inside ACARS msg_text.
// dumphfdl does not parse weather content — it passes msg_text verbatim.
// We filter for label "H1" + sublabel "WX" and store the last N messages
// in a ring buffer, served via GET /weather.
// ---------------------------------------------------------------------------

const maxWeatherMessages = 200

// WeatherMessage is one decoded weather ACARS message.
type WeatherMessage struct {
	Time     int64  `json:"time"` // unix seconds
	FreqKHz  int64  `json:"freq_khz"`
	Reg      string `json:"reg,omitempty"`
	Flight   string `json:"flight,omitempty"`
	GSID     int    `json:"gs_id,omitempty"`
	Label    string `json:"label"`    // always "H1"
	Sublabel string `json:"sublabel"` // always "WX"
	MsgText  string `json:"msg_text"`
}

// weatherSnapshot returns a copy of the weather ring buffer, newest first.
func (s *statsStore) weatherSnapshot() []WeatherMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.weatherMessages) == 0 {
		return []WeatherMessage{}
	}
	cp := make([]WeatherMessage, len(s.weatherMessages))
	copy(cp, s.weatherMessages)
	return cp
}
