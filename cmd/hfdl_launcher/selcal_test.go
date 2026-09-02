package main

import (
	"encoding/binary"
	"math"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Signal synthesis
// ---------------------------------------------------------------------------

const testRate = 12000

// toneHz returns the tabulated frequency of a SELCAL designator.
func toneHz(t *testing.T, ch rune) float64 {
	t.Helper()
	for _, e := range selcalToneTable {
		if e.Char == ch {
			return e.Hz
		}
	}
	t.Fatalf("unknown SELCAL designator %q", ch)
	return 0
}

// synthBurst builds an audio buffer containing one SELCAL transmission:
// leading noise, pulse 1, the inter-pulse gap, pulse 2, trailing noise.
// offsetHz shifts every tone by the same amount, standing in for a receiver or
// transmitter frequency error.
func synthBurst(t *testing.T, p1, p2 [2]rune, offsetHz, snrLinear float64) []float64 {
	t.Helper()
	rnd := rand.New(rand.NewSource(1))

	var out []float64
	noise := func(secs float64) {
		for i := 0; i < int(secs*testRate); i++ {
			out = append(out, rnd.NormFloat64()*0.002)
		}
	}
	pulse := func(pair [2]rune, secs float64) {
		f1 := toneHz(t, pair[0]) + offsetHz
		f2 := toneHz(t, pair[1]) + offsetHz
		base := len(out) // fixed before appending, or the phase advances twice per sample
		n := int(secs * testRate)
		for i := 0; i < n; i++ {
			ts := float64(base+i) / testRate
			v := snrLinear * (math.Sin(2*math.Pi*f1*ts) + math.Sin(2*math.Pi*f2*ts))
			out = append(out, v+rnd.NormFloat64()*0.002)
		}
	}

	noise(0.5)
	pulse(p1, 1.0)
	noise(0.2)
	pulse(p2, 1.0)
	noise(0.6)
	return out
}

// decodeAll runs a buffer through a fresh detector in realistic packet-sized
// chunks and returns every burst it reports.
func decodeAll(samples []float64) []selcalDetection {
	d := newSelcalDetector(testRate)
	var got []selcalDetection
	const chunk = 960 // 80 ms, similar to a receiver audio packet
	for i := 0; i < len(samples); i += chunk {
		end := i + chunk
		if end > len(samples) {
			end = len(samples)
		}
		got = append(got, d.feed(samples[i:end])...)
	}
	return got
}

// ---------------------------------------------------------------------------
// Tone table
// ---------------------------------------------------------------------------

func TestToneTableMatchesAnnex10(t *testing.T) {
	if len(selcalToneTable) != 32 {
		t.Fatalf("expected 32 tones, got %d", len(selcalToneTable))
	}

	// Every designator appears once, and none of the excluded letters (I, N, O)
	// are present.
	seen := map[rune]bool{}
	for _, e := range selcalToneTable {
		if seen[e.Char] {
			t.Errorf("duplicate designator %q", e.Char)
		}
		seen[e.Char] = true
	}
	for _, bad := range []rune{'I', 'N', 'O', '0'} {
		if seen[bad] {
			t.Errorf("designator %q must not be in the table", bad)
		}
	}

	// Annex 10 Table 3-1 Note 1: frequencies are spaced by antilog 0.0225.
	// Sorting by frequency must reproduce that geometric progression, which
	// independently validates every value in the table.
	freqs := make([]float64, len(selcalToneTable))
	for i, e := range selcalToneTable {
		freqs[i] = e.Hz
	}
	for i := 0; i < len(freqs); i++ {
		for j := i + 1; j < len(freqs); j++ {
			if freqs[j] < freqs[i] {
				freqs[i], freqs[j] = freqs[j], freqs[i]
			}
		}
	}
	want := math.Pow(10, 0.0225)
	for i := 1; i < len(freqs); i++ {
		ratio := freqs[i] / freqs[i-1]
		if math.Abs(ratio-want) > 0.0015 {
			t.Errorf("spacing %g→%g Hz is %.5f, want %.5f", freqs[i-1], freqs[i], ratio, want)
		}
	}

	// The narrowest gap drives the offset tolerance, so keep them consistent.
	if gap := freqs[1] - freqs[0]; math.Abs(gap-selcalMinToneGapHz) > 0.2 {
		t.Errorf("selcalMinToneGapHz = %g, but the table's minimum gap is %g", selcalMinToneGapHz, gap)
	}
	if selcalMaxOffsetHz >= selcalMinToneGapHz/2 {
		t.Errorf("selcalMaxOffsetHz (%g) must stay below half the minimum tone gap (%g) "+
			"or a shifted tone could map to the wrong neighbour",
			selcalMaxOffsetHz, selcalMinToneGapHz/2)
	}
}

func TestSelcalCodeOrdering(t *testing.T) {
	idx := map[rune]int{}
	for i, e := range selcalToneTable {
		idx[e.Char] = i
	}

	// Characters within each pulse are written in ascending designator order
	// regardless of the order the tones were detected in.
	if got := selcalCode([2]int{idx['C'], idx['A']}, [2]int{idx['D'], idx['B']}); got != "AC-BD" {
		t.Errorf("selcalCode = %q, want %q", got, "AC-BD")
	}
	// A repeated tone is not a legal code.
	if got := selcalCode([2]int{idx['A'], idx['B']}, [2]int{idx['A'], idx['D']}); got != "" {
		t.Errorf("repeated tone should be rejected, got %q", got)
	}
	// Designator order, not frequency order: '9' (1557.8 Hz) sorts after 'S'
	// (1479.1 Hz), but '1' (680.0 Hz) also sorts after 'S'.
	if got := selcalCode([2]int{idx['S'], idx['1']}, [2]int{idx['A'], idx['9']}); got != "S1-A9" {
		t.Errorf("selcalCode = %q, want %q", got, "S1-A9")
	}
}

// ---------------------------------------------------------------------------
// Detector
// ---------------------------------------------------------------------------

func TestDecodeClassicSelcal(t *testing.T) {
	got := decodeAll(synthBurst(t, [2]rune{'A', 'B'}, [2]rune{'C', 'D'}, 0, 0.3))
	if len(got) != 1 {
		t.Fatalf("expected 1 detection, got %d (%+v)", len(got), got)
	}
	if got[0].Code != "AB-CD" {
		t.Errorf("code = %q, want %q", got[0].Code, "AB-CD")
	}
	if got[0].Selcal32 {
		t.Error("AB-CD uses only classic tones and must not be flagged SELCAL32")
	}
}

func TestDecodeSelcal32(t *testing.T) {
	// T and 9 are from the expanded set; A and S are classic.  T (329.2 Hz)
	// sits between A (312.6) and B (346.7), so this also exercises the
	// tightest discrimination in the table.
	got := decodeAll(synthBurst(t, [2]rune{'A', 'T'}, [2]rune{'S', '9'}, 0, 0.3))
	if len(got) != 1 {
		t.Fatalf("expected 1 detection, got %d (%+v)", len(got), got)
	}
	// Pulse 1 carries A+T and pulse 2 carries S+9; each pair is written in
	// designator order, where T and 9 follow S.
	if got[0].Code != "AT-S9" {
		t.Errorf("code = %q, want %q", got[0].Code, "AT-S9")
	}
	if !got[0].Selcal32 {
		t.Error("AT-S9 contains expanded tones and must be flagged SELCAL32")
	}
}

func TestDecodeToleratesSmallFrequencyOffset(t *testing.T) {
	// A few Hz of common offset — a slightly off-channel station — must still
	// decode, and the offset should be reported.
	got := decodeAll(synthBurst(t, [2]rune{'A', 'B'}, [2]rune{'C', 'D'}, 3.0, 0.3))
	if len(got) != 1 {
		t.Fatalf("expected 1 detection, got %d (%+v)", len(got), got)
	}
	if math.Abs(got[0].OffsetHz-3.0) > 1.5 {
		t.Errorf("reported offset %.2f Hz, want ≈3.0", got[0].OffsetHz)
	}
}

func TestRejectsLargeFrequencyOffset(t *testing.T) {
	// A 40 Hz error exceeds anything a real channel should show and would make
	// tone identification ambiguous, so it must be rejected rather than
	// silently decoded as some other code.
	if got := decodeAll(synthBurst(t, [2]rune{'A', 'B'}, [2]rune{'C', 'D'}, 40.0, 0.3)); len(got) != 0 {
		t.Errorf("expected no detection at 40 Hz offset, got %+v", got)
	}
}

func TestRejectsNoise(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	samples := make([]float64, 5*testRate)
	for i := range samples {
		samples[i] = rnd.NormFloat64() * 0.05
	}
	if got := decodeAll(samples); len(got) != 0 {
		t.Errorf("noise produced %d spurious detection(s): %+v", len(got), got)
	}
}

func TestRejectsSingleTone(t *testing.T) {
	// A carrier or a single steady tone is not SELCAL: the detector requires
	// two comparable peaks.
	rnd := rand.New(rand.NewSource(3))
	samples := make([]float64, 0, 4*testRate)
	for i := 0; i < 4*testRate; i++ {
		ts := float64(i) / testRate
		samples = append(samples, 0.3*math.Sin(2*math.Pi*1000*ts)+rnd.NormFloat64()*0.002)
	}
	if got := decodeAll(samples); len(got) != 0 {
		t.Errorf("single tone produced %d spurious detection(s): %+v", len(got), got)
	}
}

func TestRejectsSinglePulse(t *testing.T) {
	// One valid pulse with no partner must not produce a code.
	rnd := rand.New(rand.NewSource(5))
	var samples []float64
	for i := 0; i < int(0.5*testRate); i++ {
		samples = append(samples, rnd.NormFloat64()*0.002)
	}
	f1, f2 := toneHz(t, 'A'), toneHz(t, 'B')
	base := len(samples)
	for i := 0; i < testRate; i++ {
		ts := float64(base+i) / testRate
		samples = append(samples, 0.3*(math.Sin(2*math.Pi*f1*ts)+math.Sin(2*math.Pi*f2*ts)))
	}
	for i := 0; i < int(1.5*testRate); i++ {
		samples = append(samples, rnd.NormFloat64()*0.002)
	}
	if got := decodeAll(samples); len(got) != 0 {
		t.Errorf("single pulse produced %d spurious detection(s): %+v", len(got), got)
	}
}

func TestDecodesAtLowSNR(t *testing.T) {
	// Tones only modestly above the noise floor should still decode; SELCAL is
	// transmitted by the ground station, so in practice it arrives strong.
	got := decodeAll(synthBurst(t, [2]rune{'E', 'K'}, [2]rune{'M', 'R'}, 0, 0.02))
	if len(got) != 1 || got[0].Code != "EK-MR" {
		t.Errorf("expected EK-MR at low SNR, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Relay codec
// ---------------------------------------------------------------------------

// ulawDecode mirrors the lookup table selcal.js builds in the browser.
func ulawDecode(b byte) int32 {
	u := ^b
	mantissa := int32(u & 0x0F)
	exponent := uint((u >> 4) & 0x07)
	sample := (((mantissa << 3) + 0x84) << exponent) - 0x84
	if u&0x80 != 0 {
		return -sample
	}
	return sample
}

func TestULawRoundTrip(t *testing.T) {
	// µ-law is lossy by design; what matters is that the error stays small
	// relative to the sample, so speech and tones survive the relay intact.
	for v := -32000; v <= 32000; v += 37 {
		got := ulawDecode(linearToULaw(int16(v)))
		tolerance := int32(math.Abs(float64(v))*0.08) + 40
		if diff := got - int32(v); diff > tolerance || diff < -tolerance {
			t.Fatalf("µ-law round trip of %d gave %d (error %d, tolerance %d)", v, got, diff, tolerance)
		}
	}
	// Full-scale and zero must not wrap.
	for _, v := range []int16{-32768, -32767, 0, 32767} {
		if got := ulawDecode(linearToULaw(v)); math.Abs(float64(got)-float64(v)) > 2100 {
			t.Errorf("µ-law round trip of %d gave %d", v, got)
		}
	}
}

func TestULawSurvivesToneDetection(t *testing.T) {
	// The detector runs on the original samples, but confirm the companded
	// relay is still good enough to decode from — a listener recording the
	// stream should get the same answer.
	src := synthBurst(t, [2]rune{'A', 'B'}, [2]rune{'C', 'D'}, 0, 0.3)
	companded := make([]float64, len(src))
	for i, v := range src {
		companded[i] = float64(ulawDecode(linearToULaw(int16(v*32767)))) / 32768
	}
	got := decodeAll(companded)
	if len(got) != 1 || got[0].Code != "AB-CD" {
		t.Errorf("expected AB-CD through the µ-law relay, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestParseSelcalFreqs(t *testing.T) {
	got, err := parseSelcalFreqs("8906:NAT-A, 5598 , 5505:Shannon Volmet:audio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 channels, got %d", len(got))
	}
	// Sorted by frequency.
	if got[0].FreqKHz != 5505 || got[1].FreqKHz != 5598 || got[2].FreqKHz != 8906 {
		t.Errorf("channels not sorted by frequency: %+v", got)
	}
	if got[0].Label != "Shannon Volmet" || got[0].Decode {
		t.Errorf("expected an audio-only Shannon Volmet channel, got %+v", got[0])
	}
	if !got[1].Decode || got[1].Label != "" {
		t.Errorf("bare frequency should decode with no label, got %+v", got[1])
	}
	if got[2].FreqHz() != 8906000 {
		t.Errorf("FreqHz = %d, want 8906000", got[2].FreqHz())
	}

	if _, err := parseSelcalFreqs(""); err != nil {
		t.Errorf("empty config should be valid (feature disabled), got %v", err)
	}
	for _, bad := range []string{"abc", "150:TooLow", "8906,8906"} {
		if _, err := parseSelcalFreqs(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Receiver protocol
// ---------------------------------------------------------------------------

// buildPCMPacket assembles a protocol version 4 packet carrying the given
// samples verbatim.
//
// It uses the escape body -- the format's own path for samples a predictor
// cannot help, such as full-entropy noise -- rather than implementing the
// predictive encoder here. That keeps the fixture honest about the parts these
// tests are about (the header, the metadata carry-forward and the signal
// quality) while still driving the decoder's real escape path, which advances
// the filters exactly as a coded packet would.
//
// The bit-exactness of the codec itself is covered where it belongs, in
// internal/pcmv4 against a stream the server's own encoder produced.
//
// withQuality selects whether the header carries a signal-quality reading, the
// way a server does only when it has one.
func buildPCMPacket(t *testing.T, withQuality bool, rate, channels int,
	power, noise float32, samples []int16) []byte {
	t.Helper()

	const (
		flagEscape   = 1 << 7
		flagQuality  = 1 << 6
		flagMetadata = 1 << 5
		flagCount    = 1 << 3

		// Profile lives in the low three bits and is declared by the packet, not
		// inferred. 1 is the real cascade for mono audio; 0 is the complex IQ
		// filter, which requires whole two-channel frames and rejects an odd
		// sample count.
		profileAudio = 1
		profileIQ    = 0
	)

	profile := byte(profileAudio)
	if channels == 2 {
		profile = profileIQ
	}

	// Metadata makes this a resynchronisation point: absolute timestamp, and
	// the rate and channel count stated rather than carried forward.
	flags := byte(flagEscape|flagMetadata|flagCount) | profile
	if withQuality {
		flags |= flagQuality
	}

	buf := make([]byte, 0, 32+len(samples)*2)
	buf = binary.LittleEndian.AppendUint32(buf, 0x344D4350) // "PCM4"
	buf = append(buf, flags)
	buf = binary.LittleEndian.AppendUint64(buf, 1234) // absolute timestamp
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(scratch[:], uint64(len(samples)))
	buf = append(buf, scratch[:n]...)
	n = binary.PutUvarint(scratch[:], uint64(rate))
	buf = append(buf, scratch[:n]...)
	buf = append(buf, byte(channels))
	if withQuality {
		// Centidecibels, as the wire carries them.
		buf = binary.LittleEndian.AppendUint16(buf, uint16(int16(math.Round(float64(power)*100))))
		buf = binary.LittleEndian.AppendUint16(buf, uint16(int16(math.Round(float64(noise)*100))))
	}
	// Escape bodies carry little-endian int16 samples.
	for _, sm := range samples {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(sm))
	}
	return buf
}

func TestDecodePacketCarriesSignalQuality(t *testing.T) {
	d, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer d.close()

	want := []int16{0, 1, -1, 32767, -32768, 1234}
	pkt, err := d.decode(buildPCMPacket(t, true, 12000, 1, -42.5, -88.25, want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pkt.Rate != 12000 || pkt.Channels != 1 {
		t.Errorf("rate/channels = %d/%d, want 12000/1", pkt.Rate, pkt.Channels)
	}
	if !pkt.HasSignal {
		t.Fatal("a quality-bearing header should carry signal quality")
	}
	if pkt.BasebandPowerDB != -42.5 || pkt.NoiseDB != -88.25 {
		t.Errorf("signal = %g/%g dBFS, want -42.5/-88.25", pkt.BasebandPowerDB, pkt.NoiseDB)
	}
	for i, s := range want {
		if pkt.Samples[i] != s {
			t.Fatalf("sample %d = %d, want %d", i, pkt.Samples[i], s)
		}
	}
}

// Version 4 carries signal quality FORWARD, the way it carries the rate and
// channel count: a packet without the quality bit means "unchanged", not "no
// reading". That differs from versions 1 and 2, where a minimal header simply
// had no fields and the level went unknown between full headers -- which is why
// the launcher can now show a level for every channel continuously.
func TestDecodePacketCarriesQualityForward(t *testing.T) {
	d, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer d.close()

	if _, err := d.decode(buildPCMPacket(t, true, 12000, 1, -42.5, -88.25, []int16{1, 2, 3})); err != nil {
		t.Fatalf("priming decode: %v", err)
	}
	pkt, err := d.decode(buildPCMPacket(t, false, 12000, 1, 0, 0, []int16{4, 5, 6}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !pkt.HasSignal {
		t.Error("quality should be carried forward from the last packet that stated it")
	}
	if pkt.BasebandPowerDB != -42.5 || pkt.NoiseDB != -88.25 {
		t.Errorf("carried-forward signal = %g/%g dBFS, want -42.5/-88.25",
			pkt.BasebandPowerDB, pkt.NoiseDB)
	}
	if pkt.Rate != 12000 || pkt.Channels != 1 {
		t.Errorf("rate/channels = %d/%d, want 12000/1", pkt.Rate, pkt.Channels)
	}
	if len(pkt.Samples) != 3 {
		t.Errorf("got %d samples, want 3", len(pkt.Samples))
	}
}

func TestSignalUnavailableSentinel(t *testing.T) {
	d, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer d.close()

	// -32768 centidecibels is the codepoint for "radiod reported nothing"; it
	// reaches the caller as -999 and must not be shown as a level.
	pkt, err := d.decode(buildPCMPacket(t, true, 12000, 1, -327.68, -327.68, []int16{1, 2}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pkt.HasSignal {
		t.Error("the 'unavailable' sentinel must not be reported as a level")
	}
}

// A server too old for version 4 answers with a zstd frame rather than
// refusing. Recognising it is what turns a dead channel into a message.
func TestLegacyServerFrameIsNamed(t *testing.T) {
	d, err := newPCMDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer d.close()

	zstdFrame := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00}
	_, err = d.decode(zstdFrame)
	if err == nil {
		t.Fatal("expected an error for a pre-version-4 server's frame")
	}
	if !strings.Contains(err.Error(), "0.1.63") {
		t.Errorf("error should name the required server version, got: %v", err)
	}
}

func TestChannelWSURL(t *testing.T) {
	ch := newSelcalChannel(
		selcalChannelCfg{FreqKHz: 8906, Label: "NAT-A", Decode: true},
		"http://ubersdr:8080", "", nil, newListenerBudget(0), "", 8, false)

	u, err := url.Parse(ch.wsURL("sess-1"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"frequency":     "8906000", // carrier frequency, as aeronautical channels are quoted
		"mode":          "usb",
		"format":        "pcm-zstd",
		"version":       "4", // predictive codec, and the signal-quality fields
		"bandwidthLow":  "50",
		"bandwidthHigh": "2700", // passband contains every SELCAL tone (312.6–1557.8 Hz)
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if u.Scheme != "ws" || u.Host != "ubersdr:8080" || u.Path != "/ws" {
		t.Errorf("unexpected endpoint %s://%s%s", u.Scheme, u.Host, u.Path)
	}
}

func TestListenerBudgetIsSharedAcrossChannels(t *testing.T) {
	// Upload bandwidth is a receiver-wide resource, so the cap must apply to
	// the total, not per channel.
	budget := newListenerBudget(2)
	a, b := newAudioHub(budget), newAudioHub(budget)

	s1, ok1 := a.subscribe()
	_, ok2 := b.subscribe()
	if !ok1 || !ok2 {
		t.Fatal("first two listeners should be admitted")
	}
	if _, ok := b.subscribe(); ok {
		t.Error("third listener should be refused: the budget spans all channels")
	}
	a.unsubscribe(s1)
	if _, ok := b.subscribe(); !ok {
		t.Error("releasing a listener on one channel should free capacity on another")
	}
	if got := budget.current(); got != 2 {
		t.Errorf("budget.current() = %d, want 2", got)
	}
	// Unsubscribing twice must not leak capacity back.
	a.unsubscribe(s1)
	if got := budget.current(); got != 2 {
		t.Errorf("double unsubscribe changed the budget to %d, want 2", got)
	}
}

// TestShippedDefaultFreqsAreValid parses SELCAL_FREQS straight out of the
// docker-compose.yml that install.sh downloads, so the shipped default cannot
// drift into something the launcher would reject at startup.
func TestShippedDefaultFreqsAreValid(t *testing.T) {
	data, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Skipf("docker-compose.yml not readable: %v", err)
	}

	var spec string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SELCAL_FREQS:") { // an active setting, not a "# SELCAL_FREQS:" comment
			spec = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "SELCAL_FREQS:")), `"`)
			break
		}
	}
	if spec == "" {
		t.Skip("no active SELCAL_FREQS in docker-compose.yml")
	}

	got, err := parseSelcalFreqs(spec)
	if err != nil {
		t.Fatalf("shipped SELCAL_FREQS %q does not parse: %v", spec, err)
	}
	if len(got) == 0 {
		t.Fatal("shipped SELCAL_FREQS parsed to no channels")
	}
	for _, c := range got {
		if c.FreqKHz < 2000 || c.FreqKHz > 30000 {
			t.Errorf("%g kHz is outside the HF range", c.FreqKHz)
		}
		if c.Label == "" {
			t.Errorf("%g kHz has no label — the UI shows these to users", c.FreqKHz)
		}
	}
	t.Logf("shipped default: %d channels, one UberSDR session each", len(got))
}

// TestSpotSignalComesFromReceiverDuringBurst checks that a decoded call is
// quoted with the receiver's own calibrated level for the moment it was
// transmitted — not the quiet level either side of it, and not the detector's
// FFT-derived margin, which sits on an entirely different scale.
func TestSpotSignalComesFromReceiverDuringBurst(t *testing.T) {
	const quietSNR, burstSNR = 34.0, 58.0

	store := newSelcalStore(true, func(string) {})
	ch := newSelcalChannel(
		selcalChannelCfg{FreqKHz: 8906, Label: "NAT-A", Decode: true},
		"http://receiver.invalid", "", store, newListenerBudget(0), "", 8, false)

	samples := synthBurst(t, [2]rune{'A', 'B'}, [2]rune{'C', 'D'}, 0, 0.3)

	// Feed it as packets, labelling each with the level the receiver would have
	// reported: quiet either side, loud while the tones are actually playing.
	// synthBurst lays out 0.5 s lead-in, 1.0 s pulse, 0.2 s gap, 1.0 s pulse.
	const pkt = 240 // 20 ms at 12 kHz, as the receiver sends
	floats := make([]float64, 0, pkt)
	for i := 0; i < len(samples); i += pkt {
		end := i + pkt
		if end > len(samples) {
			end = len(samples)
		}
		sec := float64(i) / testRate
		snr := quietSNR
		if sec >= 0.5 && sec <= 2.7 {
			snr = burstSNR
		}
		pcm := make([]int16, end-i)
		for j, v := range samples[i:end] {
			pcm[j] = int16(v * 32767)
		}
		ch.handlePacket(&pcmPacket{
			Samples:         pcm,
			Rate:            testRate,
			Channels:        1,
			BasebandPowerDB: -100 + snr, // power − noise = snr
			NoiseDB:         -100,
			HasSignal:       true,
		}, &floats)
	}

	store.flush("AB-CD")
	snap := store.snapshot(nil)
	if len(snap.Spots) != 1 {
		t.Fatalf("expected 1 decoded call, got %d", len(snap.Spots))
	}
	spot := snap.Spots[0]

	if !spot.HasSNR {
		t.Fatal("spot carries no receiver signal level")
	}
	if math.Abs(spot.SNRDB-burstSNR) > 0.5 {
		t.Errorf("spot signal = %.1f dB, want the burst level %.1f (not the quiet %.1f)",
			spot.SNRDB, burstSNR, quietSNR)
	}
	// The margin is a separate, uncalibrated figure and must not have been
	// substituted for the signal level.
	if spot.MarginDB == spot.SNRDB {
		t.Error("margin and signal are identical — the two measurements have been conflated")
	}
	if spot.MarginDB <= 0 {
		t.Errorf("margin = %g, want a positive decode margin", spot.MarginDB)
	}
}

// TestSpotSignalAbsentWithoutReceiverData covers an older server that sends no
// signal-quality fields: the call must still decode, and simply report no level
// rather than inventing one.
func TestSpotSignalAbsentWithoutReceiverData(t *testing.T) {
	store := newSelcalStore(true, func(string) {})
	ch := newSelcalChannel(
		selcalChannelCfg{FreqKHz: 8906, Decode: true},
		"http://receiver.invalid", "", store, newListenerBudget(0), "", 8, false)

	samples := synthBurst(t, [2]rune{'A', 'B'}, [2]rune{'C', 'D'}, 0, 0.3)
	floats := make([]float64, 0, 240)
	for i := 0; i < len(samples); i += 240 {
		end := i + 240
		if end > len(samples) {
			end = len(samples)
		}
		pcm := make([]int16, end-i)
		for j, v := range samples[i:end] {
			pcm[j] = int16(v * 32767)
		}
		ch.handlePacket(&pcmPacket{Samples: pcm, Rate: testRate, Channels: 1}, &floats)
	}

	store.flush("AB-CD")
	snap := store.snapshot(nil)
	if len(snap.Spots) != 1 {
		t.Fatalf("expected 1 decoded call, got %d", len(snap.Spots))
	}
	if snap.Spots[0].HasSNR {
		t.Error("reported a signal level despite the receiver supplying none")
	}
	if snap.Spots[0].MarginDB <= 0 {
		t.Error("decode margin should still be reported")
	}
}
