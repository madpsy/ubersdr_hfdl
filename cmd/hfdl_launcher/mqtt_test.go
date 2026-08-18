package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// ── Endpoint derivation ─────────────────────────────────────────────────────

func TestMQTTIngestBaseURL(t *testing.T) {
	t.Setenv("UBERSDR_INGEST_URL", "")

	cases := []struct{ in, want string }{
		{"http://ubersdr:8080", "http://ubersdr:6926"},
		{"http://ubersdr:8080/", "http://ubersdr:6926"},
		{"https://sdr.example.com", "http://sdr.example.com:6926"},
		{"http://192.168.1.10:8073/some/path", "http://192.168.1.10:6926"},
		{"http://user:pw@ubersdr:8080", "http://ubersdr:6926"},
		{"http://[fd00::1]:8080", "http://[fd00::1]:6926"},
		{"ubersdr:8080", "http://ubersdr:6926"}, // no scheme
		{"", "http://ubersdr:6926"},
	}
	for _, tc := range cases {
		if got := mqttIngestBaseURL(tc.in); got != tc.want {
			t.Errorf("mqttIngestBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMQTTIngestBaseURLOverride(t *testing.T) {
	t.Setenv("UBERSDR_INGEST_URL", "http://elsewhere:7000/")
	if got := mqttIngestBaseURL("http://ubersdr:8080"); got != "http://elsewhere:7000" {
		t.Errorf("override = %q", got)
	}
}

// ── Sanitiser ───────────────────────────────────────────────────────────────

func TestMQTTSanitise(t *testing.T) {
	if got := mqttSanitise("HELLO 123", 0); got != "HELLO 123" {
		t.Errorf("plain ASCII = %q", got)
	}
	if got := mqttSanitise("a\nb\tc", 0); got != "a\nb\tc" {
		t.Errorf("whitespace should survive: %q", got)
	}

	// ACARS off a noisy HF link can carry arbitrary bytes; those are not valid
	// UTF-8 and would break JSON parsing downstream in Home Assistant.
	nasty := "OK\xff\x80\x01END"
	got := mqttSanitise(nasty, 0)
	if got != "OK???END" {
		t.Errorf("garbled bytes = %q, want %q", got, "OK???END")
	}
	if !utf8.ValidString(got) {
		t.Error("sanitised output must be valid UTF-8")
	}

	if got := mqttSanitise("abcdefghij", 4); got != "abcd" {
		t.Errorf("truncation = %q", got)
	}
	// Truncation must not be able to split a multi-byte sequence, because the
	// input is already reduced to single-byte ASCII before the cut.
	if !utf8.ValidString(mqttSanitise("\xe2\x82\xacabc", 2)) {
		t.Error("truncated output must remain valid UTF-8")
	}
}

// ── Rate limiting ───────────────────────────────────────────────────────────

func TestMQTTBucketLimitsAndRefills(t *testing.T) {
	b := newMqttBucket(60) // 60/min == 1/s
	now := time.Now()

	// A full bucket permits a burst up to its size, then stops.
	allowed := 0
	for i := 0; i < 100; i++ {
		if b.allow(now) {
			allowed++
		}
	}
	if allowed != 60 {
		t.Errorf("burst allowed %d, want 60 (the bucket size)", allowed)
	}
	if b.drops() != 40 {
		t.Errorf("drops = %d, want 40", b.drops())
	}

	// It refills over time rather than staying latched shut.
	if !b.allow(now.Add(2 * time.Second)) {
		t.Error("bucket should have refilled after 2 s")
	}

	// Refill is capped at the bucket size, not unbounded.
	later := now.Add(time.Hour)
	allowed = 0
	for i := 0; i < 200; i++ {
		if b.allow(later) {
			allowed++
		}
	}
	if allowed > 61 {
		t.Errorf("refill overshot the cap: allowed %d after an hour idle", allowed)
	}
}

func TestMQTTTopicRatesFitIngestBudget(t *testing.T) {
	// The receiver's default ingest allowance is 120 publishes/min, shared with
	// every other addon. Two summaries a minute come out of the same budget.
	total := 2.0
	for _, r := range mqttTopicRates {
		total += r
	}
	if total > 120 {
		t.Errorf("topic rates sum to %.0f/min, exceeding the 120/min ingest default", total)
	}
	if total > 80 {
		t.Errorf("topic rates sum to %.0f/min, leaving too little headroom for other addons", total)
	}
}

// ── Declarations ────────────────────────────────────────────────────────────

func TestMQTTDeclarationsMatchServerRules(t *testing.T) {
	var (
		entityKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
		iconRe      = regexp.MustCompile(`^mdi:[a-z0-9-]{1,40}$`)

		components  = map[string]bool{"sensor": true, "binary_sensor": true}
		stateClass  = map[string]bool{"measurement": true, "total": true, "total_increasing": true}
		entityCat   = map[string]bool{"diagnostic": true}
		deviceClass = map[string]bool{"timestamp": true, "connectivity": true, "problem": true, "distance": true}
	)

	seen := map[string]bool{}
	for _, e := range mqttEntities() {
		str := func(k string) string { v, _ := e[k].(string); return v }
		key := str("entity_key")

		if !entityKeyRe.MatchString(key) {
			t.Errorf("bad entity_key %q", key)
		}
		if !components[str("component")] {
			t.Errorf("%s: bad component %q", key, str("component"))
		}
		if n := str("name"); n == "" || len(n) > 64 {
			t.Errorf("%s: bad name %q", key, n)
		}
		for _, f := range []string{"value_template", "json_attributes_template"} {
			if len(str(f)) > 256 {
				t.Errorf("%s: %s is %d chars (max 256)", key, f, len(str(f)))
			}
		}
		if u := str("unit_of_measurement"); len(u) > 16 {
			t.Errorf("%s: unit %q too long", key, u)
		}
		if i := str("icon"); i != "" && !iconRe.MatchString(i) {
			t.Errorf("%s: bad icon %q", key, i)
		}
		if c := str("entity_category"); c != "" && !entityCat[c] {
			t.Errorf("%s: bad entity_category %q", key, c)
		}
		if d := str("device_class"); d != "" && !deviceClass[d] {
			t.Errorf("%s: device_class %q not in the set this test knows about", key, d)
		}
		if sc := str("state_class"); sc != "" {
			if !stateClass[sc] {
				t.Errorf("%s: bad state_class %q", key, sc)
			}
			if str("component") != "sensor" {
				t.Errorf("%s: state_class is sensor-only", key)
			}
		}
		if _, ok := e["sub_topic"]; ok {
			t.Errorf("%s: sub_topic is set by declareEntities, not the table", key)
		}
		if seen[key] {
			t.Errorf("duplicate entity_key %q", key)
		}
		seen[key] = true
	}

	if len(seen) == 0 {
		t.Fatal("no entities declared")
	}
	// The receiver caps entities per addon at 20 by default.
	if len(seen) > 20 {
		t.Errorf("%d entities exceeds the receiver's default max_entities of 20", len(seen))
	}
}

// TestNoPerAircraftEntities guards the design decision: aircraft churn while
// Home Assistant's entity registry is permanent, so nothing may declare an
// entity keyed on an individual airframe.
func TestNoPerAircraftEntities(t *testing.T) {
	for _, e := range mqttEntities() {
		key, _ := e["entity_key"].(string)
		tmpl, _ := e["value_template"].(string)
		if strings.Contains(tmpl, "value_json.aircraft[") || strings.Contains(key, "icao") {
			t.Errorf("%s looks like a per-aircraft entity", key)
		}
	}
}

// ── Fake ingest ─────────────────────────────────────────────────────────────

type fakeIngest struct {
	mu           sync.Mutex
	declarations []map[string]any
	published    map[string][]map[string]any
	retained     map[string]bool
	haDiscovery  bool
	healthStatus int
	srv          *httptest.Server
}

func newFakeIngest(t *testing.T) *fakeIngest {
	t.Helper()
	f := &fakeIngest{
		published:    map[string][]map[string]any{},
		retained:     map[string]bool{},
		haDiscovery:  true,
		healthStatus: http.StatusOK,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		status, ha := f.healthStatus, f.haDiscovery
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"addon": "hfdl", "mqtt_connected": true, "ha_discovery": ha,
			"rate_limit": 120, "offline_after_sec": 300,
		})
	})
	mux.HandleFunc("/discovery", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.declarations = append(f.declarations, d)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "declared"})
	})
	mux.HandleFunc("/publish/", func(w http.ResponseWriter, r *http.Request) {
		sub := strings.TrimPrefix(r.URL.Path, "/publish/")
		raw, _ := io.ReadAll(r.Body)
		if !utf8.Valid(raw) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.published[sub] = append(f.published[sub], body)
		if r.URL.Query().Get("retain") == "true" {
			f.retained[sub] = true
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "published"})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	t.Setenv("UBERSDR_INGEST_URL", f.srv.URL)
	return f
}

func (f *fakeIngest) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published[sub])
}

func (f *fakeIngest) last(sub string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.published[sub]
	if len(msgs) == 0 {
		return nil
	}
	return msgs[len(msgs)-1]
}

func (f *fakeIngest) waitFor(sub string, n int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(sub) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func newTestPublisher(t *testing.T, summary MQTTSummaryFunc) *MQTTPublisher {
	t.Helper()
	p := NewMQTTPublisher("http://ubersdr:8080", summary)
	p.Start()
	t.Cleanup(p.Stop)
	return p
}

// ── Publishing ──────────────────────────────────────────────────────────────

func TestPublishesDeclarationsAndSummary(t *testing.T) {
	fake := newFakeIngest(t)
	p := newTestPublisher(t, func() map[string]any {
		return map[string]any{"messages_total": 42}
	})

	if !p.isAvailable() {
		t.Fatal("publisher should be available")
	}
	fake.mu.Lock()
	got := len(fake.declarations)
	fake.mu.Unlock()
	if want := len(mqttEntities()); got != want {
		t.Errorf("declared %d entities, want %d", got, want)
	}

	if !fake.waitFor("summary", 1) {
		t.Fatal("no summary published")
	}
	fake.mu.Lock()
	retained := fake.retained["summary"]
	fake.mu.Unlock()
	if !retained {
		t.Error("summary must be retained")
	}
}

func TestPublishACARS(t *testing.T) {
	fake := newFakeIngest(t)
	p := newTestPublisher(t, func() map[string]any { return map[string]any{} })

	p.PublishACARS(RecentMessage{
		Time: 1755500000, FreqKHz: 10081, SigLevel: -18.5,
		Flight: "BAW117", Reg: "G-STBA", SrcICAO: "4008F3",
		Label: "H1", MsgText: "POS N51.5 W000.1\xffFL380",
	})
	if !fake.waitFor("acars", 1) {
		t.Fatal("ACARS message was never published")
	}

	m := fake.last("acars")
	if m["flight"] != "BAW117" || m["reg"] != "G-STBA" {
		t.Errorf("identity fields wrong: %v", m)
	}
	if m["freq_khz"] != float64(10081) {
		t.Errorf("freq_khz = %v", m["freq_khz"])
	}
	text, _ := m["text"].(string)
	if !strings.Contains(text, "POS N51.5") {
		t.Errorf("text missing: %q", text)
	}
	if strings.ContainsRune(text, 0xFFFD) || !utf8.ValidString(text) {
		t.Errorf("text not sanitised: %q", text)
	}
	if strings.Contains(text, "\xff") {
		t.Error("raw high byte survived into the payload")
	}

	// Events must not be retained: replaying a stale ACARS message is wrong.
	fake.mu.Lock()
	retained := fake.retained["acars"]
	fake.mu.Unlock()
	if retained {
		t.Error("acars must not be retained")
	}

	// A message with no text is not an ACARS event at all.
	before := fake.count("acars")
	p.PublishACARS(RecentMessage{Flight: "BAW117"})
	time.Sleep(100 * time.Millisecond)
	if fake.count("acars") != before {
		t.Error("a message with no text should not publish")
	}
}

func TestPublishAircraftAndSelcalFeedSummary(t *testing.T) {
	fake := newFakeIngest(t)
	p := newTestPublisher(t, func() map[string]any {
		return map[string]any{"messages_total": 1}
	})

	p.PublishAircraftNew(AircraftState{
		Key: "4008F3", ICAO: "4008F3", Reg: "G-STBA", Flight: "BAW117",
		Lat: 51.5, Lon: -0.1, FreqKHz: 10081, GSID: 1, LastSeen: 1755500000,
		AltFt: 38000, AltValid: true,
	})
	if !fake.waitFor("aircraft", 1) {
		t.Fatal("aircraft sighting was never published")
	}
	ac := fake.last("aircraft")
	if ac["icao"] != "4008F3" || ac["lat"] != 51.5 || ac["alt_ft"] != float64(38000) {
		t.Errorf("aircraft payload wrong: %v", ac)
	}

	p.PublishSelcal(SelcalSpot{
		Time: 1755500100, Code: "AB-CD", Selcal32: false,
		SNRDB: 12.5, HasSNR: true, MarginDB: 20.1,
		Channels: []SelcalSpotChannel{
			{FreqKHz: 8864, Label: "NAT-B", SNRDB: 12.5, HasSNR: true, MarginDB: 20.1},
			{FreqKHz: 8906, Label: "NAT-A", MarginDB: 17.3},
		},
	})
	if !fake.waitFor("selcal", 1) {
		t.Fatal("SELCAL spot was never published")
	}
	sc := fake.last("selcal")
	if sc["code"] != "AB-CD" {
		t.Errorf("selcal code = %v", sc["code"])
	}
	freqs, _ := sc["freqs_khz"].([]any)
	if len(freqs) != 2 {
		t.Errorf("selcal should list every channel that carried it, got %v", sc["freqs_khz"])
	}

	// Both must surface in the retained summary for the HA entities to read.
	p.publishSummary()
	s := fake.last("summary")
	if s["last_aircraft"] != "BAW117" {
		t.Errorf("last_aircraft = %v", s["last_aircraft"])
	}
	if s["last_selcal"] != "AB-CD" {
		t.Errorf("last_selcal = %v", s["last_selcal"])
	}
	if _, ok := s["last_aircraft_detail"]; !ok {
		t.Error("last_aircraft_detail missing")
	}
}

// TestSummaryResolvesEveryEntityTemplate is the guard against a declaration
// that is accepted by the receiver but silently shows nothing because the field
// it reads is never published.
func TestSummaryResolvesEveryEntityTemplate(t *testing.T) {
	fake := newFakeIngest(t)
	p := newTestPublisher(t, func() map[string]any {
		return map[string]any{
			"messages_total":        10,
			"messages_per_min":      1.5,
			"aircraft_active":       3,
			"ground_stations_heard": 2,
			"frequencies_active":    4,
			"furthest_km":           1234.5,
			"decoders_running":      2,
			"decoder_problem":       false,
		}
	})

	p.PublishACARS(RecentMessage{Time: 1, Flight: "BAW117", MsgText: "HI"})
	p.PublishAircraftNew(AircraftState{Key: "X", Flight: "BAW117"})
	p.PublishSelcal(SelcalSpot{Code: "AB-CD"})
	p.publishSummary()

	s := fake.last("summary")
	if s == nil {
		t.Fatal("no summary")
	}

	fieldRe := regexp.MustCompile(`value_json\.(\w+)`)
	for _, e := range mqttEntities() {
		tmpl, _ := e["value_template"].(string)
		for _, m := range fieldRe.FindAllStringSubmatch(tmpl, -1) {
			if _, ok := s[m[1]]; !ok {
				t.Errorf("%v: summary has no field %q", e["entity_key"], m[1])
			}
		}
	}
}

func TestDropsAreReportedNotHidden(t *testing.T) {
	fake := newFakeIngest(t)
	p := newTestPublisher(t, func() map[string]any { return map[string]any{} })

	// Exhaust the ACARS bucket well past its ceiling.
	for i := 0; i < int(mqttTopicRates["acars"])+50; i++ {
		p.PublishACARS(RecentMessage{Time: int64(i), Flight: "X", MsgText: "T"})
	}
	p.publishSummary()

	s := fake.last("summary")
	dropped, _ := s["dropped_events"].(float64)
	if dropped < 50 {
		t.Errorf("dropped_events = %v, want at least 50 — a curated feed must not look complete", dropped)
	}
	detail, _ := s["dropped_detail"].(map[string]any)
	if _, ok := detail["acars"]; !ok {
		t.Errorf("dropped_detail should attribute drops to the topic: %v", detail)
	}
}

func TestDormantWhenNotRecognised(t *testing.T) {
	fake := newFakeIngest(t)
	fake.mu.Lock()
	fake.healthStatus = http.StatusForbidden
	fake.mu.Unlock()

	p := newTestPublisher(t, func() map[string]any { return map[string]any{} })
	if p.isAvailable() {
		t.Fatal("publisher must stay dormant on 403")
	}
	p.PublishACARS(RecentMessage{MsgText: "HI"})
	time.Sleep(100 * time.Millisecond)
	if fake.count("acars") != 0 {
		t.Error("dormant publisher must not publish")
	}
}

func TestRecoversWhenReceiverComesUp(t *testing.T) {
	fake := newFakeIngest(t)
	fake.mu.Lock()
	fake.healthStatus = http.StatusServiceUnavailable
	fake.mu.Unlock()

	p := NewMQTTPublisher("http://ubersdr:8080", func() map[string]any { return map[string]any{} })
	if p.probe() {
		t.Fatal("probe should fail while the receiver is down")
	}

	fake.mu.Lock()
	fake.healthStatus = http.StatusOK
	fake.mu.Unlock()

	p.publishSummary()
	if !p.isAvailable() {
		t.Fatal("publisher should recover on the next summary")
	}
	fake.mu.Lock()
	n := len(fake.declarations)
	fake.mu.Unlock()
	if n != len(mqttEntities()) {
		t.Errorf("declared %d after recovery, want %d", n, len(mqttEntities()))
	}
}

func TestHADiscoveryDisabledStillPublishes(t *testing.T) {
	fake := newFakeIngest(t)
	fake.mu.Lock()
	fake.haDiscovery = false
	fake.mu.Unlock()

	p := newTestPublisher(t, func() map[string]any { return map[string]any{} })

	fake.mu.Lock()
	n := len(fake.declarations)
	fake.mu.Unlock()
	if n != 0 {
		t.Error("must not declare entities when HA discovery is off")
	}
	p.PublishACARS(RecentMessage{Time: 1, Flight: "X", MsgText: "HI"})
	if !fake.waitFor("acars", 1) {
		t.Error("data publishing must still work without HA discovery")
	}
}

func TestNilPublisherIsSafe(t *testing.T) {
	var p *MQTTPublisher
	p.Start()
	p.PublishACARS(RecentMessage{MsgText: "x"})
	p.PublishAircraftNew(AircraftState{})
	p.PublishSelcal(SelcalSpot{})
	p.PublishEvent("logon", nil)
	p.Stop()
}

// ── Event classification ────────────────────────────────────────────────────

func TestLogonEventKind(t *testing.T) {
	cases := map[string]string{
		"Logon request":    "logon",
		"Logon confirm":    "logon",
		"logon resume":     "logon",
		"Logoff request":   "logoff",
		"LOGOFF":           "logoff",
		"Unnumbered data":  "",
		"Performance data": "",
		"":                 "",
	}
	for in, want := range cases {
		if got := logonEventKind(in); got != want {
			t.Errorf("logonEventKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Stats aggregation ───────────────────────────────────────────────────────

func TestMqttAggregates(t *testing.T) {
	s := newStatsStore(map[int]string{1: "Shannon"}, nil, nil)
	now := time.Now().Unix()

	s.total = 500
	s.dumphfdlVer = "1.4.1"
	s.heardGS[1] = now
	s.heardGS[2] = now - aircraftMaxAgeSecs - 60 // stale, must not count

	// Near and far aircraft, plus one stale that must be excluded entirely.
	s.aircraft["NEAR"] = &AircraftState{Key: "NEAR", ICAO: "NEAR", Lat: 51.5, Lon: -0.2, LastSeen: now}
	s.aircraft["FAR"] = &AircraftState{
		Key: "FAR", ICAO: "FAR", Flight: "QFA1", Lat: -33.9, Lon: 151.2,
		LastSeen: now, AltFt: 39000, AltValid: true,
	}
	s.aircraft["STALE"] = &AircraftState{Key: "STALE", Lat: 40, Lon: 40, LastSeen: now - aircraftMaxAgeSecs - 60}

	s.freqs[10081000] = &FreqStats{FreqHz: 10081000, FreqKHz: 10081, GSStats: map[int]*GSFreqStats{
		1: {GSID: 1, MsgCount: 300, LastSeen: now, AvgSigLevel: -18.0},
	}}
	// A frequency with no messages must not count as active.
	s.freqs[8977000] = &FreqStats{FreqHz: 8977000, FreqKHz: 8977, GSStats: map[int]*GSFreqStats{}}

	out := s.mqttAggregates(51.5, -0.1) // receiver in London

	if out["messages_total"] != int64(500) {
		t.Errorf("messages_total = %v", out["messages_total"])
	}
	if out["aircraft_active"] != 2 {
		t.Errorf("aircraft_active = %v, want 2 (stale excluded)", out["aircraft_active"])
	}
	if out["ground_stations_heard"] != 1 {
		t.Errorf("ground_stations_heard = %v, want 1 (stale excluded)", out["ground_stations_heard"])
	}
	if out["frequencies_active"] != 1 {
		t.Errorf("frequencies_active = %v, want 1 (silent frequency excluded)", out["frequencies_active"])
	}
	if out["dumphfdl_version"] != "1.4.1" {
		t.Errorf("dumphfdl_version = %v", out["dumphfdl_version"])
	}

	// London → Sydney is roughly 17,000 km; the near aircraft must not win.
	km, _ := out["furthest_km"].(float64)
	if km < 16000 || km > 18000 {
		t.Errorf("furthest_km = %v, want ~17000", km)
	}
	detail, _ := out["furthest_detail"].(map[string]any)
	if detail["flight"] != "QFA1" {
		t.Errorf("furthest_detail should name the far aircraft: %v", detail)
	}

	freqs, _ := out["frequencies"].(map[string]any)
	row, _ := freqs["10081"].(map[string]any)
	if row["messages"] != int64(300) {
		t.Errorf("per-frequency rollup wrong: %v", freqs)
	}

	// The whole payload must serialise — it goes out as JSON every 30 s.
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("summary does not encode: %v", err)
	}
}

func TestMqttAggregatesWithoutReceiverPosition(t *testing.T) {
	s := newStatsStore(nil, nil, nil)
	s.aircraft["A"] = &AircraftState{Key: "A", Lat: 51.5, Lon: -0.1, LastSeen: time.Now().Unix()}

	out := s.mqttAggregates(0, 0)
	if _, ok := out["furthest_km"]; ok {
		t.Error("furthest_km must be omitted when the receiver position is unknown, not reported as 0")
	}
	if out["aircraft_active"] != 1 {
		t.Errorf("aircraft_active = %v", out["aircraft_active"])
	}
}
