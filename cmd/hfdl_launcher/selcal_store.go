package main

// selcal_store.go — SELCAL spot storage, cross-channel de-duplication and
// the /selcal snapshot.
//
// Ground stations key SELCAL simultaneously on several frequencies of whichever
// family is in use, so one call to one aircraft typically lands on more than
// one monitored channel within a couple of seconds.  Those are collapsed into
// a single spot listing every frequency it was heard on — which is not only
// tidier, but turns each call into a simultaneous propagation measurement from
// a transmitter at a known location.

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// selcalDedupeWindow is how long a detection is held open for the same code to
// arrive on other channels before the spot is published.  Comfortably longer
// than the spread introduced by per-channel buffering, and short enough that
// two genuinely separate calls of the same code are not merged.
const selcalDedupeWindow = 2500 * time.Millisecond

// selcalMaxSpots caps the in-memory spot history.
const selcalMaxSpots = 500

// SelcalSpotChannel records one channel that carried a burst.
type SelcalSpotChannel struct {
	FreqKHz float64 `json:"freq_khz"`
	Label   string  `json:"label,omitempty"`
	SNRDB   float64 `json:"snr_db"`
}

// SelcalSpot is one decoded SELCAL call, merged across channels.
type SelcalSpot struct {
	Time     int64               `json:"time"`
	Code     string              `json:"code"`
	Selcal32 bool                `json:"selcal32"`
	SNRDB    float64             `json:"snr_db"`
	OffsetHz float64             `json:"offset_hz"`
	Channels []SelcalSpotChannel `json:"channels"`
}

// SelcalChannelSummary augments the live channel status with decode history.
type SelcalChannelSummary struct {
	selcalChannelStatus
	LastCode string `json:"last_code,omitempty"`
	LastTime int64  `json:"last_time,omitempty"`
	Count    int64  `json:"count"`
}

// SelcalSnapshot is the payload served at /selcal.
type SelcalSnapshot struct {
	Enabled   bool                   `json:"enabled"`
	Audio     bool                   `json:"audio"`
	StartTime int64                  `json:"start_time"`
	Total     int64                  `json:"total"`
	Channels  []SelcalChannelSummary `json:"channels"`
	Spots     []SelcalSpot           `json:"spots"`

	// Listener accounting is receiver-wide, because upload bandwidth is shared
	// across channels rather than divided between them.
	ListenersTotal int `json:"listeners_total"`
	ListenersMax   int `json:"listeners_max"`   // 0 = unlimited
	ListenerKbps   int `json:"listener_kbps"`   // per listener, µ-law
	UploadKbps     int `json:"upload_kbps"`     // currently committed
	UploadMaxKbps  int `json:"upload_max_kbps"` // at the listener cap
}

// pendingSpot accumulates detections of one code during the dedupe window.
type pendingSpot struct {
	spot  SelcalSpot
	timer *time.Timer
}

// selcalStore holds decoded spots and per-channel history.
type selcalStore struct {
	mu        sync.Mutex
	spots     []SelcalSpot // newest last
	total     int64
	startTime time.Time
	pending   map[string]*pendingSpot

	lastCode map[string]string // channel ID → most recent code
	lastTime map[string]int64
	counts   map[string]int64

	audioEnabled bool
	broadcast    func(string)
}

func newSelcalStore(audioEnabled bool, broadcast func(string)) *selcalStore {
	return &selcalStore{
		startTime:    time.Now(),
		pending:      make(map[string]*pendingSpot),
		lastCode:     make(map[string]string),
		lastTime:     make(map[string]int64),
		counts:       make(map[string]int64),
		audioEnabled: audioEnabled,
		broadcast:    broadcast,
	}
}

// record ingests one detection, merging it into an open pending spot for the
// same code or opening a new one.
func (s *selcalStore) record(cfg selcalChannelCfg, det selcalDetection, when time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := cfg.ID()
	s.lastCode[id] = det.Code
	s.lastTime[id] = when.Unix()
	s.counts[id]++

	entry := SelcalSpotChannel{FreqKHz: cfg.FreqKHz, Label: cfg.Label, SNRDB: round1(det.SNRDB)}

	if p, ok := s.pending[det.Code]; ok {
		// Same code already in flight — add this channel to it.
		for _, c := range p.spot.Channels {
			if c.FreqKHz == cfg.FreqKHz {
				return // already recorded from this channel
			}
		}
		p.spot.Channels = append(p.spot.Channels, entry)
		if det.SNRDB > p.spot.SNRDB {
			p.spot.SNRDB = round1(det.SNRDB)
		}
		return
	}

	p := &pendingSpot{
		spot: SelcalSpot{
			Time:     when.Unix(),
			Code:     det.Code,
			Selcal32: det.Selcal32,
			SNRDB:    round1(det.SNRDB),
			OffsetHz: round1(det.OffsetHz),
			Channels: []SelcalSpotChannel{entry},
		},
	}
	p.timer = time.AfterFunc(selcalDedupeWindow, func() { s.flush(det.Code) })
	s.pending[det.Code] = p
}

// flush publishes a pending spot once its dedupe window has closed.
func (s *selcalStore) flush(code string) {
	s.mu.Lock()
	p, ok := s.pending[code]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.pending, code)

	spot := p.spot
	s.spots = append(s.spots, spot)
	if len(s.spots) > selcalMaxSpots {
		s.spots = s.spots[len(s.spots)-selcalMaxSpots:]
	}
	s.total++
	broadcast := s.broadcast
	s.mu.Unlock()

	if broadcast != nil {
		if payload, err := json.Marshal(sseEvent{Type: "selcal", Data: spot}); err == nil {
			broadcast(string(payload))
		} else {
			log.Printf("selcal: marshal spot: %v", err)
		}
	}
}

// snapshot builds the /selcal payload, newest spots first.
func (s *selcalStore) snapshot(statuses []selcalChannelStatus) SelcalSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := SelcalSnapshot{
		Enabled:   len(statuses) > 0,
		Audio:     s.audioEnabled,
		StartTime: s.startTime.Unix(),
		Total:     s.total,
		Channels:  make([]SelcalChannelSummary, 0, len(statuses)),
		Spots:     make([]SelcalSpot, 0, len(s.spots)),
	}

	for _, st := range statuses {
		snap.Channels = append(snap.Channels, SelcalChannelSummary{
			selcalChannelStatus: st,
			LastCode:            s.lastCode[st.ID],
			LastTime:            s.lastTime[st.ID],
			Count:               s.counts[st.ID],
		})
	}

	for i := len(s.spots) - 1; i >= 0; i-- {
		snap.Spots = append(snap.Spots, s.spots[i])
	}
	return snap
}

func round1(v float64) float64 {
	return float64(int64(v*10+sign(v)*0.5)) / 10
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
