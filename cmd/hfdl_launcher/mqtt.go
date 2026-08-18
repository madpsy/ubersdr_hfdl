package main

// mqtt.go — publishes curated HFDL activity to UberSDR's MQTT feed, and exposes
// it in Home Assistant.
//
// UberSDR runs an ingest port on the sdr-network that lets addons publish
// through the receiver's own MQTT connection, and declare Home Assistant
// entities, without holding broker credentials. See addon_mqtt.md in the
// ka9q_ubersdr repository.
//
// Not optional and needs no configuration: the endpoint is derived from the
// UberSDR URL this launcher already uses, so the existing docker-compose.yml is
// sufficient. When the receiver has MQTT disabled, or this container is not a
// recognised addon, publishing is skipped and decoding is unaffected.
//
// ── Why this feed is curated rather than complete ──────────────────────────
//
// HFDL squitters arrive on a fixed ~32 s cadence per ground station per
// frequency, so the raw message floor scales with (ground stations heard ×
// frequencies monitored) and can approach the receiver's whole ingest budget on
// its own — while carrying nothing anyone would read. Publishing every message
// is therefore not an option, and the interesting content is a small fraction
// of the traffic. Four event streams carry that fraction:
//
//	acars     — messages carrying operator text
//	aircraft  — FIRST sighting of an airframe, not every message from it
//	selcal    — selective calls, already deduplicated across channels
//	events    — logon/logoff and ground-station state changes
//
// plus one retained `summary` holding counters and last-seen state, which is
// what every Home Assistant entity reads.
//
// Each event stream has its own token bucket so a burst on one cannot starve
// the others or the summary. Drops are counted and surfaced in the summary
// rather than hidden.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// mqttDefaultIngestPort is UberSDR's mqtt.addon_ingest.port default.
	mqttDefaultIngestPort = "6926"

	mqttAddonVersion = "1.1.0"
	mqttAddonModel   = "HFDL decoder and aircraft tracker"

	// Must stay well below the receiver's offline_after_sec (default 300) or it
	// would mark us offline between updates and Home Assistant would flap.
	mqttSummaryInterval = 30 * time.Second

	// Bounded so a stalled receiver cannot grow memory without limit.
	mqttQueueMax = 256

	mqttRequestTimeout = 5 * time.Second

	// ACARS text is third-party content off a noisy HF link; cap what goes on
	// the wire, and cap harder for the Home Assistant attribute, which lives in
	// the recorder.
	mqttACARSTextMax    = 1024
	mqttACARSPreviewMax = 256
)

// Per-topic publish ceilings, messages per minute. The sum sits comfortably
// inside the ingest default of 120/min, leaving headroom for other addons
// sharing the receiver's MQTT connection.
var mqttTopicRates = map[string]float64{
	"acars":    30,
	"aircraft": 10,
	"selcal":   10,
	"events":   10,
}

// ── Endpoint derivation ─────────────────────────────────────────────────────

// mqttIngestBaseURL works out where UberSDR's addon ingest port is.
//
// Derived from the UberSDR base URL this launcher already talks to, so a stock
// docker-compose.yml needs no new variables. UBERSDR_INGEST_URL overrides it
// for the rare case where the operator has moved the port.
//
// Always plain http: the ingest port is reachable only from the sdr-network, so
// an https base URL for the public interface is irrelevant here.
func mqttIngestBaseURL(ubersdrURL string) string {
	if v := strings.TrimRight(os.Getenv("UBERSDR_INGEST_URL"), "/"); v != "" {
		return v
	}

	host := ""
	if u, err := url.Parse(ubersdrURL); err == nil {
		host = u.Hostname()
	}
	if host == "" {
		// Bare "host:port" or "host" parses with an empty Hostname; salvage it
		// rather than silently falling back to the compose default.
		s := ubersdrURL
		if i := strings.Index(s, "://"); i >= 0 {
			s = s[i+3:]
		}
		s = strings.TrimSuffix(s, "/")
		if i := strings.IndexAny(s, "/?#"); i >= 0 {
			s = s[:i]
		}
		if i := strings.LastIndex(s, "@"); i >= 0 {
			s = s[i+1:]
		}
		if h, _, err := net.SplitHostPort(s); err == nil {
			host = h
		} else {
			host = s
		}
	}
	if host == "" {
		host = "ubersdr"
	}
	return "http://" + net.JoinHostPort(host, mqttDefaultIngestPort)
}

// mqttSanitise reduces text to printable ASCII.
//
// ACARS text arrives off a noisy HF link, so a corrupt decode can carry
// arbitrary bytes. Those are not valid UTF-8, and a JSON string must be: left
// alone they would reach the broker but fail to parse in Home Assistant.
func mqttSanitise(s string, max int) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n' || c == '\r' || c == '\t':
			b.WriteByte(c)
		case c >= 0x20 && c < 0x7F:
			b.WriteByte(c)
		default:
			b.WriteByte('?')
		}
	}
	out := b.String()
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// ── Token bucket ────────────────────────────────────────────────────────────

// mqttBucket is a per-topic rate limiter. Refills continuously so a short burst
// is allowed but a sustained flood is not.
type mqttBucket struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	perSec   float64
	lastFill time.Time
	dropped  uint64
}

func newMqttBucket(perMinute float64) *mqttBucket {
	return &mqttBucket{
		tokens:   perMinute,
		max:      perMinute,
		perSec:   perMinute / 60.0,
		lastFill: time.Now(),
	}
}

// allow consumes a token, reporting whether the caller may publish.
func (b *mqttBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.perSec
		if b.tokens > b.max {
			b.tokens = b.max
		}
		b.lastFill = now
	}
	if b.tokens < 1 {
		b.dropped++
		return false
	}
	b.tokens--
	return true
}

func (b *mqttBucket) drops() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// ── Publisher ───────────────────────────────────────────────────────────────

// mqttJob is one queued publish.
type mqttJob struct {
	topic   string
	payload any
	retain  bool
}

// MQTTSummaryFunc supplies the retained summary payload. Injected rather than
// reaching into the stats store directly, so the publisher stays independent of
// what it is reporting on and can be tested on its own.
type MQTTSummaryFunc func() map[string]any

// MQTTPublisher publishes curated HFDL activity to UberSDR's ingest port.
// A nil *MQTTPublisher is usable — every method is a no-op — so callers never
// need to check availability.
type MQTTPublisher struct {
	base    string
	client  *http.Client
	summary MQTTSummaryFunc

	mu        sync.Mutex
	available bool
	warned    bool

	buckets map[string]*mqttBucket

	queue    chan mqttJob
	queueDrp uint64

	// Last-of-each-kind, tracked here rather than in the stats store: these are
	// properties of what was PUBLISHED, and two of the three (ACARS, SELCAL)
	// come from different stores that have no reason to know about each other.
	lastAircraft map[string]any
	lastACARS    map[string]any
	lastSelcal   map[string]any

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewMQTTPublisher builds a publisher. Call Start to probe and begin serving.
func NewMQTTPublisher(ubersdrURL string, summary MQTTSummaryFunc) *MQTTPublisher {
	buckets := make(map[string]*mqttBucket, len(mqttTopicRates))
	for topic, rate := range mqttTopicRates {
		buckets[topic] = newMqttBucket(rate)
	}
	return &MQTTPublisher{
		base:    mqttIngestBaseURL(ubersdrURL),
		client:  &http.Client{Timeout: mqttRequestTimeout},
		summary: summary,
		buckets: buckets,
		queue:   make(chan mqttJob, mqttQueueMax),
		stop:    make(chan struct{}),
	}
}

// Start probes the receiver and launches the publisher and summary goroutines.
func (p *MQTTPublisher) Start() {
	if p == nil {
		return
	}
	log.Printf("mqtt: ingest endpoint %s", p.base)
	p.probe()

	p.wg.Add(2)
	go p.runPublisher()
	go p.runSummary()
}

// Stop shuts the publisher down.
func (p *MQTTPublisher) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()
}

func (p *MQTTPublisher) isAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.available
}

// unavailable records that publishing is off, logging only on a state change.
func (p *MQTTPublisher) unavailable(format string, args ...any) {
	p.mu.Lock()
	p.available = false
	warn := !p.warned
	p.warned = true
	p.mu.Unlock()

	if warn {
		log.Printf("mqtt: "+format, args...)
	}
}

// probe checks reachability and declares Home Assistant entities on the
// transition to available — covering both first start and the receiver having
// MQTT enabled later.
func (p *MQTTPublisher) probe() bool {
	resp, err := p.client.Get(p.base + "/health")
	if err != nil {
		p.unavailable("ingest port unreachable (%v) — continuing without MQTT", err)
		return false
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden:
		p.unavailable("not a recognised UberSDR addon — continuing without MQTT")
		return false
	case resp.StatusCode != http.StatusOK:
		p.unavailable("ingest health returned %s — continuing without MQTT", resp.Status)
		return false
	}

	var health struct {
		Addon           string `json:"addon"`
		MQTTConnected   bool   `json:"mqtt_connected"`
		HADiscovery     bool   `json:"ha_discovery"`
		RateLimit       int    `json:"rate_limit"`
		OfflineAfterSec int    `json:"offline_after_sec"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&health); err != nil {
		p.unavailable("could not parse ingest health (%v) — continuing without MQTT", err)
		return false
	}

	p.mu.Lock()
	already := p.available
	p.available = true
	p.warned = false
	p.mu.Unlock()
	if already {
		return true
	}

	log.Printf("mqtt: connected as addon %q (broker=%v, ha_discovery=%v, rate_limit=%d/min)",
		health.Addon, health.MQTTConnected, health.HADiscovery, health.RateLimit)

	if n := health.OfflineAfterSec; n > 0 && time.Duration(n)*time.Second < 2*mqttSummaryInterval {
		log.Printf("mqtt: warning — receiver marks addons offline after %ds but we publish every %s",
			n, mqttSummaryInterval)
	}

	if health.HADiscovery {
		p.declareEntities()
	} else {
		log.Printf("mqtt: Home Assistant discovery disabled on the receiver — publishing data only")
	}
	return true
}

// ── Home Assistant ──────────────────────────────────────────────────────────

// mqttEntities are the entities this addon exposes. All read the retained
// summary topic and are distinguished by entity_key, so one publish updates
// every one of them.
//
// There are deliberately no per-aircraft entities: aircraft churn constantly
// while Home Assistant's entity registry is permanent, so per-airframe entities
// would accumulate thousands of dead rows. Aircraft are events; the summary
// reports aggregates and the most recent one.
func mqttEntities() []map[string]any {
	return []map[string]any{
		{
			"entity_key": "messages_total", "component": "sensor",
			"name":                "Messages Received",
			"value_template":      "{{ value_json.messages_total }}",
			"unit_of_measurement": "messages",
			"state_class":         "total_increasing",
			"icon":                "mdi:radio-tower",
		},
		{
			"entity_key": "message_rate", "component": "sensor",
			"name":                "Message Rate",
			"value_template":      "{{ value_json.messages_per_min }}",
			"unit_of_measurement": "msg/min",
			"state_class":         "measurement",
			"icon":                "mdi:speedometer",
		},
		{
			"entity_key": "aircraft_tracked", "component": "sensor",
			"name":                "Aircraft Tracked",
			"value_template":      "{{ value_json.aircraft_active }}",
			"unit_of_measurement": "aircraft",
			"state_class":         "measurement",
			"icon":                "mdi:airplane",
		},
		{
			"entity_key": "ground_stations", "component": "sensor",
			"name":                "Ground Stations Heard",
			"value_template":      "{{ value_json.ground_stations_heard }}",
			"unit_of_measurement": "stations",
			"state_class":         "measurement",
			"icon":                "mdi:antenna",
		},
		{
			"entity_key": "frequencies_active", "component": "sensor",
			"name":                "Frequencies Active",
			"value_template":      "{{ value_json.frequencies_active }}",
			"unit_of_measurement": "channels",
			"state_class":         "measurement",
			"icon":                "mdi:sine-wave",
		},
		{
			"entity_key": "last_aircraft", "component": "sensor",
			"name":                     "Last Aircraft",
			"value_template":           "{{ value_json.last_aircraft | default('', true) }}",
			"json_attributes_template": "{{ value_json.last_aircraft_detail | default({}, true) | tojson }}",
			"icon":                     "mdi:airplane-marker",
		},
		{
			"entity_key": "last_acars", "component": "sensor",
			"name":                     "Last ACARS",
			"value_template":           "{{ value_json.last_acars | default('', true) }}",
			"json_attributes_template": "{{ value_json.last_acars_detail | default({}, true) | tojson }}",
			"icon":                     "mdi:message-text",
		},
		{
			"entity_key": "last_selcal", "component": "sensor",
			"name":                     "Last SELCAL",
			"value_template":           "{{ value_json.last_selcal | default('', true) }}",
			"json_attributes_template": "{{ value_json.last_selcal_detail | default({}, true) | tojson }}",
			"icon":                     "mdi:phone-in-talk",
		},
		{
			"entity_key": "furthest_aircraft", "component": "sensor",
			"name":                     "Furthest Aircraft",
			"value_template":           "{{ value_json.furthest_km | default('', true) }}",
			"unit_of_measurement":      "km",
			"device_class":             "distance",
			"state_class":              "measurement",
			"json_attributes_template": "{{ value_json.furthest_detail | default({}, true) | tojson }}",
			"icon":                     "mdi:map-marker-distance",
		},
		{
			"entity_key": "decoders_running", "component": "sensor",
			"name":                "Decoders Running",
			"value_template":      "{{ value_json.decoders_running }}",
			"unit_of_measurement": "decoders",
			"state_class":         "measurement",
			"entity_category":     "diagnostic",
			"icon":                "mdi:cog",
		},
		{
			"entity_key": "decoder_problem", "component": "binary_sensor",
			"name":            "Decoder Problem",
			"value_template":  "{{ 'ON' if value_json.decoder_problem else 'OFF' }}",
			"device_class":    "problem",
			"entity_category": "diagnostic",
		},
	}
}

func (p *MQTTPublisher) declareEntities() {
	entities := mqttEntities()
	declared := 0

	for _, e := range entities {
		body := make(map[string]any, len(e)+4)
		for k, v := range e {
			body[k] = v
		}
		body["sub_topic"] = "summary"
		body["addon_version"] = mqttAddonVersion
		body["addon_model"] = mqttAddonModel

		raw, err := json.Marshal(body)
		if err != nil {
			log.Printf("mqtt: declare %v: %v", e["entity_key"], err)
			continue
		}
		resp, err := p.client.Post(p.base+"/discovery", "application/json", bytes.NewReader(raw))
		if err != nil {
			log.Printf("mqtt: declare %v: %v", e["entity_key"], err)
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Printf("mqtt: declare %v rejected (%s): %s",
				e["entity_key"], resp.Status, bytes.TrimSpace(msg))
			continue
		}
		declared++
	}
	log.Printf("mqtt: declared %d/%d Home Assistant entities", declared, len(entities))
}

// ── Event API ───────────────────────────────────────────────────────────────
//
// All of these are called from the decode path and never block: they enqueue
// and return.

// PublishACARS publishes an ACARS message carrying operator text.
func (p *MQTTPublisher) PublishACARS(rm RecentMessage) {
	if p == nil || rm.MsgText == "" {
		return
	}
	text := mqttSanitise(rm.MsgText, mqttACARSTextMax)

	who := rm.Flight
	if who == "" {
		who = rm.Reg
	}
	if who == "" {
		who = rm.SrcICAO
	}
	p.setLast("acars", who, map[string]any{
		"flight":   rm.Flight,
		"reg":      rm.Reg,
		"icao":     rm.SrcICAO,
		"label":    rm.Label,
		"freq_khz": rm.FreqKHz,
		"time":     rm.Time,
		"text":     mqttSanitise(rm.MsgText, mqttACARSPreviewMax),
	})

	p.enqueue("acars", map[string]any{
		"time":      rm.Time,
		"freq_khz":  rm.FreqKHz,
		"sig_level": rm.SigLevel,
		"flight":    rm.Flight,
		"reg":       rm.Reg,
		"icao":      rm.SrcICAO,
		"label":     rm.Label,
		"sublabel":  rm.Sublabel,
		"msg_type":  rm.MsgType,
		"text":      text,
	})
}

// PublishAircraftNew publishes the first sighting of an airframe.
// Emphatically not every message from it — that is the difference between a
// handful of events an hour and swamping the ingest budget.
func (p *MQTTPublisher) PublishAircraftNew(ac AircraftState) {
	if p == nil {
		return
	}
	payload := map[string]any{
		"time":     ac.LastSeen,
		"key":      ac.Key,
		"icao":     ac.ICAO,
		"reg":      ac.Reg,
		"flight":   ac.Flight,
		"freq_khz": ac.FreqKHz,
		"gs_id":    ac.GSID,
	}
	if ac.SigLevel != 0 {
		payload["sig_level"] = ac.SigLevel
	}
	if isValidPos(ac.Lat, ac.Lon) {
		payload["lat"] = ac.Lat
		payload["lon"] = ac.Lon
	}
	if ac.AltValid {
		payload["alt_ft"] = ac.AltFt
	}

	who := ac.Flight
	if who == "" {
		who = ac.Reg
	}
	if who == "" {
		who = ac.Key
	}
	p.setLast("aircraft", who, map[string]any{
		"icao":     ac.ICAO,
		"reg":      ac.Reg,
		"flight":   ac.Flight,
		"freq_khz": ac.FreqKHz,
		"gs_id":    ac.GSID,
		"time":     ac.LastSeen,
	})

	p.enqueue("aircraft", payload)
}

// PublishSelcal publishes a decoded selective call. Already deduplicated across
// channels by the store, so one call is one event however many frequencies
// carried it — and the per-channel list makes each spot a simultaneous
// multi-frequency propagation measurement.
func (p *MQTTPublisher) PublishSelcal(spot SelcalSpot) {
	if p == nil {
		return
	}
	chans := make([]map[string]any, 0, len(spot.Channels))
	freqs := make([]float64, 0, len(spot.Channels))
	for _, c := range spot.Channels {
		entry := map[string]any{"freq_khz": c.FreqKHz, "margin_db": c.MarginDB}
		if c.Label != "" {
			entry["label"] = c.Label
		}
		if c.HasSNR {
			entry["snr_db"] = c.SNRDB
		}
		chans = append(chans, entry)
		freqs = append(freqs, c.FreqKHz)
	}

	payload := map[string]any{
		"time":      spot.Time,
		"code":      spot.Code,
		"selcal32":  spot.Selcal32,
		"margin_db": spot.MarginDB,
		"freqs_khz": freqs,
		"channels":  chans,
	}
	if spot.HasSNR {
		payload["snr_db"] = spot.SNRDB
	}

	detail := map[string]any{
		"selcal32":  spot.Selcal32,
		"freqs_khz": freqs,
		"margin_db": spot.MarginDB,
		"time":      spot.Time,
	}
	if spot.HasSNR {
		detail["snr_db"] = spot.SNRDB
	}
	p.setLast("selcal", spot.Code, detail)

	p.enqueue("selcal", payload)
}

// PublishEvent publishes a network-state notification. Logon/logoff and
// ground-station change notes share one topic — both are low-rate "something
// changed" events, and one topic with a discriminator is easier to subscribe to
// than two carrying a handful of events an hour each.
func (p *MQTTPublisher) PublishEvent(kind string, fields map[string]any) {
	if p == nil {
		return
	}
	payload := map[string]any{"kind": kind}
	for k, v := range fields {
		payload[k] = v
	}
	if _, ok := payload["time"]; !ok {
		payload["time"] = time.Now().Unix()
	}
	p.enqueue("events", payload)
}

// setLast records the most recent event of a kind for the retained summary.
// state is the Home Assistant entity state (a short label); detail becomes its
// attributes.
func (p *MQTTPublisher) setLast(kind, state string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["state"] = state

	p.mu.Lock()
	switch kind {
	case "acars":
		p.lastACARS = detail
	case "aircraft":
		p.lastAircraft = detail
	case "selcal":
		p.lastSelcal = detail
	}
	p.mu.Unlock()
}

// mergeLastSeen adds the last-of-each-kind fields to a summary payload.
// Each entity reads a top-level key plus a detail object, which keeps the
// value_templates to a plain field read.
func (p *MQTTPublisher) mergeLastSeen(s map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	add := func(prefix string, d map[string]any) {
		if d == nil {
			return
		}
		if state, _ := d["state"].(string); state != "" {
			s[prefix] = state
		}
		s[prefix+"_detail"] = d
	}
	add("last_aircraft", p.lastAircraft)
	add("last_acars", p.lastACARS)
	add("last_selcal", p.lastSelcal)
}

// ── Queue and transport ─────────────────────────────────────────────────────

// enqueue applies the topic's rate limit and hands the job to the publisher
// goroutine. Never blocks — the decode path must not wait on telemetry.
func (p *MQTTPublisher) enqueue(topic string, payload map[string]any) {
	if !p.isAvailable() {
		return
	}
	if b := p.buckets[topic]; b != nil && !b.allow(time.Now()) {
		return
	}
	select {
	case p.queue <- mqttJob{topic: topic, payload: payload}:
	default:
		p.mu.Lock()
		p.queueDrp++
		p.mu.Unlock()
	}
}

func (p *MQTTPublisher) runPublisher() {
	defer p.wg.Done()
	for {
		select {
		case <-p.stop:
			return
		case job := <-p.queue:
			p.post(job.topic, job.payload, job.retain)
		}
	}
}

func (p *MQTTPublisher) runSummary() {
	defer p.wg.Done()

	// Publish once immediately so Home Assistant has values on subscribe.
	p.publishSummary()

	ticker := time.NewTicker(mqttSummaryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.publishSummary()
		}
	}
}

func (p *MQTTPublisher) publishSummary() {
	// Re-probe while dormant so the addon recovers on its own if the receiver
	// was restarted, or had MQTT enabled, after we started.
	if !p.isAvailable() && !p.probe() {
		return
	}
	if p.summary == nil {
		return
	}

	s := p.summary()
	if s == nil {
		return
	}
	p.mergeLastSeen(s)

	// Report what the rate limits and the queue discarded, so a curated feed
	// never silently looks complete.
	drops := map[string]any{}
	var total uint64
	for topic, b := range p.buckets {
		if n := b.drops(); n > 0 {
			drops[topic] = n
			total += n
		}
	}
	p.mu.Lock()
	qd := p.queueDrp
	p.mu.Unlock()
	total += qd
	if qd > 0 {
		drops["queue"] = qd
	}
	s["dropped_events"] = total
	if len(drops) > 0 {
		s["dropped_detail"] = drops
	}

	p.post("summary", s, true)
}

func (p *MQTTPublisher) post(topic string, payload any, retain bool) {
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("mqtt: marshal %s: %v", topic, err)
		return
	}

	endpoint := p.base + "/publish/" + topic
	if retain {
		endpoint += "?retain=true"
	}

	resp, err := p.client.Post(endpoint, "application/json", bytes.NewReader(raw))
	if err != nil {
		p.unavailable("publish %s failed (%v) — will retry", topic, err)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode < 300:
		return
	case resp.StatusCode == http.StatusServiceUnavailable:
		// Receiver up, broker down. Transient; stay available.
	case resp.StatusCode == http.StatusTooManyRequests:
		log.Printf("mqtt: rate limited publishing %s", topic)
	case resp.StatusCode == http.StatusForbidden:
		p.unavailable("no longer a recognised addon — pausing MQTT")
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("mqtt: publish %s: %s: %s", topic, resp.Status, bytes.TrimSpace(msg))
	}
}

// ── Receiver position ───────────────────────────────────────────────────────

// newReceiverPosFunc returns a cached lookup of the receiver's own coordinates,
// used for the furthest-aircraft figure.
//
// Cached because the receiver does not move, but retried on failure because it
// may not be up when this launcher starts. Returns 0,0 while unknown, which
// callers treat as "omit the distance" rather than as the Gulf of Guinea.
func newReceiverPosFunc(ubersdrURL string) func() (float64, float64) {
	var (
		mu      sync.Mutex
		lat     float64
		lon     float64
		haveIt  bool
		nextTry time.Time
	)
	client := &http.Client{Timeout: mqttRequestTimeout}

	return func() (float64, float64) {
		mu.Lock()
		defer mu.Unlock()

		if haveIt || time.Now().Before(nextTry) {
			return lat, lon
		}
		// Back off between attempts so a receiver without GPS configured is not
		// re-fetched on every summary tick forever.
		nextTry = time.Now().Add(10 * time.Minute)

		resp, err := client.Get(strings.TrimRight(ubersdrURL, "/") + "/api/description")
		if err != nil {
			return 0, 0
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, 0
		}

		var desc struct {
			Receiver struct {
				GPS struct {
					Lat float64 `json:"lat"`
					Lon float64 `json:"lon"`
				} `json:"gps"`
			} `json:"receiver"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&desc); err != nil {
			return 0, 0
		}
		if !isValidPos(desc.Receiver.GPS.Lat, desc.Receiver.GPS.Lon) {
			return 0, 0
		}

		lat, lon, haveIt = desc.Receiver.GPS.Lat, desc.Receiver.GPS.Lon, true
		log.Printf("mqtt: receiver position %.4f, %.4f — furthest-aircraft distance enabled", lat, lon)
		return lat, lon
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// mqttRound trims a float to the given decimal places for a tidy payload.
func mqttRound(v float64, places int) float64 {
	f := math.Pow(10, float64(places))
	return math.Round(v*f) / f
}
