package main

// selcal.go — SELCAL / SELCAL32 tone decoder.
//
// SELCAL (ICAO Annex 10 Vol III, Chapter 3) is a selective-calling scheme used
// on aeronautical HF and VHF voice channels.  A transmission consists of two
// consecutive tone pulses, each carrying two simultaneous tones:
//
//	pulse 1 (1.0 ±0.25 s)   gap (0.2 ±0.1 s)   pulse 2 (1.0 ±0.25 s)
//	 tone A + tone B                            tone C + tone D
//
// The four tones are all distinct, and the two tones within each pulse are
// written in ascending designator order — which is what makes a code
// recoverable even though the two tones are transmitted simultaneously.
//
// Classic SELCAL (ARINC 596) uses 16 tones designated A–S, spaced by
// 10^0.045 (≈10.9%).  Amendment 91 to Annex 10 Vol III — applicable from
// 3 November 2022 — halves the spacing to 10^0.0225 (≈5.32%) and interleaves
// 16 further tones designated T–Z and 1–9.  That expansion is "SELCAL32".
// Decoding classic SELCAL is a strict subset of decoding SELCAL32: the same
// detector handles both, and a code is reported as SELCAL32 when any of its
// four characters falls in the T–9 set.
//
// Detection strategy
// ------------------
// Only the ground station transmits SELCAL, so a receiver near a busy
// aeronautical station gets a strong, clean signal.  Audio arrives already
// demodulated (USB), so a tone at audio frequency f is simply a spectral line
// at f.  Each analysis frame:
//
//  1. Window with a Hann taper and take an FFT.
//  2. Find spectral peaks in the SELCAL band, refined to sub-bin accuracy by
//     parabolic interpolation on the log magnitude.
//  3. Require exactly two dominant peaks: both well above the in-band noise
//     floor, comparable in level, and clearly above the third strongest peak.
//     Voice cannot satisfy this for a full second, and the third-peak margin is
//     the main defence against harmonics and f2±f1 intermodulation products,
//     which the permitted 15% audio distortion can put close to real tone
//     frequencies.
//  4. Map each peak to the nearest tabulated tone, requiring both to share the
//     same small frequency offset (a receiver or transmitter frequency error
//     shifts every tone by the same number of Hz, so a consistent offset is
//     evidence for a real burst and an inconsistent one is evidence against).
//
// Frames carrying the same tone pair are grouped into runs, and a run pair
// separated by a plausible gap yields a code.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Tone table
// ---------------------------------------------------------------------------

// selcalTone is one entry of ICAO Annex 10 Vol III Table 3-1.
type selcalTone struct {
	Char rune
	Hz   float64
}

// selcalToneTable lists all 32 SELCAL tones in *designator* order, which is
// also the collating order used to sort the two characters within each pulse.
//
// Note this is deliberately not frequency order: the SELCAL32 tones are
// interleaved between the classic ones, so ascending frequency runs
// A T B U C V D W E X F Y G Z H 1 J 2 K 3 L 4 M 5 P 6 Q 7 R 8 S 9.
//
// Source: ICAO Annex 10, Volume III, Part II, Table 3-1 (Amendment 91,
// applicable 3 November 2022).
var selcalToneTable = []selcalTone{
	// Classic 16 (ARINC 596)
	{'A', 312.6},
	{'B', 346.7},
	{'C', 384.6},
	{'D', 426.6},
	{'E', 473.2},
	{'F', 524.8},
	{'G', 582.1},
	{'H', 645.7},
	{'J', 716.1},
	{'K', 794.3},
	{'L', 881.0},
	{'M', 977.2},
	{'P', 1083.9},
	{'Q', 1202.3},
	{'R', 1333.5},
	{'S', 1479.1},
	// SELCAL32 expansion
	{'T', 329.2},
	{'U', 365.2},
	{'V', 405.0},
	{'W', 449.3},
	{'X', 498.3},
	{'Y', 552.7},
	{'Z', 613.1},
	{'1', 680.0},
	{'2', 754.2},
	{'3', 836.6},
	{'4', 927.9},
	{'5', 1029.2},
	{'6', 1141.6},
	{'7', 1266.2},
	{'8', 1404.4},
	{'9', 1557.8},
}

// classicToneCount is the number of pre-SELCAL32 tones at the head of
// selcalToneTable.  Any tone index at or above this makes a code SELCAL32.
const classicToneCount = 16

// SELCAL band edges, with margin either side of the lowest (312.6 Hz) and
// highest (1557.8 Hz) tones.  Peak searching and the noise-floor estimate are
// both confined to this range, so out-of-band voice energy cannot desensitise
// the detector.
const (
	selcalBandLowHz  = 280.0
	selcalBandHighHz = 1620.0
)

// selcalMinToneGapHz is the smallest spacing between any two adjacent tones in
// the 32-tone table — A (312.6 Hz) to T (329.2 Hz).  The maximum tolerated
// frequency offset must stay below half of this, otherwise a shifted tone
// could be mapped to the wrong neighbour.
const selcalMinToneGapHz = 16.6

// selcalCode renders two tone-index pairs as a printable code such as "AB-CD",
// sorting each pair into designator order.  Returns "" if any index repeats,
// which the standard forbids.
func selcalCode(p1, p2 [2]int) string {
	all := []int{p1[0], p1[1], p2[0], p2[1]}
	seen := map[int]bool{}
	for _, i := range all {
		if i < 0 || i >= len(selcalToneTable) || seen[i] {
			return ""
		}
		seen[i] = true
	}
	a, b := p1[0], p1[1]
	if a > b {
		a, b = b, a
	}
	c, d := p2[0], p2[1]
	if c > d {
		c, d = d, c
	}
	return fmt.Sprintf("%c%c-%c%c",
		selcalToneTable[a].Char, selcalToneTable[b].Char,
		selcalToneTable[c].Char, selcalToneTable[d].Char)
}

// isSelcal32 reports whether any of the four tones is from the expanded set,
// meaning only aircraft with SELCAL32-capable decoders can be called with it.
func isSelcal32(p1, p2 [2]int) bool {
	for _, i := range []int{p1[0], p1[1], p2[0], p2[1]} {
		if i >= classicToneCount {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// FFT
// ---------------------------------------------------------------------------

// fftPlan holds the bit-reversal permutation and twiddle factors for a fixed
// power-of-two transform size.  Written out rather than pulled in as a
// dependency to keep the build free of new modules.
type fftPlan struct {
	n   int
	rev []int
	tw  []complex128
}

func newFFTPlan(n int) *fftPlan {
	if n&(n-1) != 0 || n < 2 {
		panic("fft size must be a power of two")
	}
	p := &fftPlan{n: n, rev: make([]int, n), tw: make([]complex128, n/2)}

	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := 0; i < n; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			if i&(1<<b) != 0 {
				r |= 1 << (bits - 1 - b)
			}
		}
		p.rev[i] = r
	}
	for i := 0; i < n/2; i++ {
		ang := -2 * math.Pi * float64(i) / float64(n)
		p.tw[i] = complex(math.Cos(ang), math.Sin(ang))
	}
	return p
}

// transform runs an in-place iterative Cooley-Tukey FFT over buf, which must
// have exactly plan.n elements.
func (p *fftPlan) transform(buf []complex128) {
	n := p.n
	for i := 0; i < n; i++ {
		if j := p.rev[i]; j > i {
			buf[i], buf[j] = buf[j], buf[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		step := n / size
		for i := 0; i < n; i += size {
			for j, k := i, 0; j < i+half; j, k = j+1, k+step {
				t := p.tw[k] * buf[j+half]
				buf[j+half] = buf[j] - t
				buf[j] += t
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Detector
// ---------------------------------------------------------------------------

// Detection thresholds and timing bounds.
//
// Annex 10 specifies pulses of 1.0 ±0.25 s separated by 0.2 ±0.1 s, but the
// bounds below are deliberately looser, because runs are measured in analysis
// window starts rather than true pulse edges.  A window straddling the end of
// a pulse still contains enough of it for the tones to dominate, so runs bleed
// outwards into the gap: the *measured* gap is shorter than the real one (and
// can reach zero), while a measured run is longer than the pulse by up to one
// window.  The overall span from the start of pulse 1 to the end of pulse 2 is
// therefore the more reliable timing discriminator, and the gap is bounded only
// from above — enough to stop unrelated bursts pairing with one another.
const (
	selcalPeakOverNoiseDB = 10.0 // weaker tone must clear the in-band noise floor by this
	selcalPairBalanceDB   = 16.0 // permitted level difference between the two tones
	selcalThirdPeakDB     = 6.0  // margin the pair must hold over any third peak
	selcalMaxOffsetHz     = 6.0  // largest tolerated common frequency error (< selcalMinToneGapHz/2)
	selcalPairOffsetHz    = 4.0  // permitted offset disagreement between the two tones of a pulse
	selcalCodeOffsetHz    = 5.0  // permitted offset disagreement across all four tones

	selcalMinRunSec  = 0.25 // shortest accepted stable-pair run
	selcalMaxRunSec  = 2.00 // longest — beyond this it is a steady signal, not a pulse
	selcalMinFrames  = 3    // frames a run must contain as well as lasting selcalMinRunSec
	selcalMaxGapSec  = 0.90 // longest accepted separation between the two pulses
	selcalMinSpanSec = 1.20 // shortest accepted pulse 1 start → pulse 2 end
	selcalMaxSpanSec = 3.50 // longest accepted pulse 1 start → pulse 2 end
)

// Reasons a frame or a run can be rejected.  Recorded so that a call which was
// plainly audible but not decoded can be explained, rather than just vanishing.
const (
	rejNone       = ""
	rejFewPeaks   = "fewer than two peaks in the SELCAL band"
	rejWeak       = "weaker tone too close to the noise floor"
	rejUnbalanced = "the two tones differ in level by too much"
	rejThirdPeak  = "a third peak too close to the pair"
	rejOffGrid    = "a peak did not land on a SELCAL tone"
	rejSameTone   = "both peaks mapped to the same tone"
	rejOffsetPair = "the two tones disagree on frequency offset"
)

// selcalDiag accumulates why frames were rejected, so a near miss can be
// explained.  Counts are since the last report.
type selcalDiag struct {
	frames  int
	reasons map[string]int
	// worst-case values seen while at least two peaks were present, which is
	// what tells you which threshold to move.
	maxImbalanceDB float64
	minThirdDB     float64
	maxOffsetHz    float64
}

func newSelcalDiag() *selcalDiag {
	return &selcalDiag{reasons: map[string]int{}, minThirdDB: math.MaxFloat64}
}

func (d *selcalDiag) note(reason string) {
	d.frames++
	if reason != rejNone {
		d.reasons[reason]++
	}
}

// summary renders the accumulated reasons, commonest first.
func (d *selcalDiag) summary() string {
	if len(d.reasons) == 0 {
		return "no rejections"
	}
	type kv struct {
		k string
		n int
	}
	var all []kv
	for k, n := range d.reasons {
		all = append(all, kv{k, n})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })

	parts := make([]string, 0, len(all))
	for _, e := range all {
		parts = append(parts, fmt.Sprintf("%s (%d)", e.k, e.n))
	}
	out := strings.Join(parts, ", ")
	if d.maxImbalanceDB > 0 {
		out += fmt.Sprintf("; worst imbalance %.1f dB (limit %.0f)", d.maxImbalanceDB, selcalPairBalanceDB)
	}
	if d.minThirdDB != math.MaxFloat64 {
		out += fmt.Sprintf("; closest third peak %.1f dB (limit %.0f)", d.minThirdDB, selcalThirdPeakDB)
	}
	if d.maxOffsetHz > 0 {
		out += fmt.Sprintf("; largest offset %.1f Hz (limit %.0f)", d.maxOffsetHz, selcalMaxOffsetHz)
	}
	return out
}

func (d *selcalDiag) reset() {
	d.frames = 0
	d.reasons = map[string]int{}
	d.maxImbalanceDB = 0
	d.minThirdDB = math.MaxFloat64
	d.maxOffsetHz = 0
}

// toneRun is a maximal sequence of consecutive frames that all resolved to the
// same tone pair — in other words, one detected pulse.
type toneRun struct {
	pair     [2]int
	startSec float64
	endSec   float64
	frames   int
	offsetHz float64 // mean frequency offset over the run
	snrDB    float64 // mean weaker-tone level above the in-band noise floor
}

// selcalDetection is one fully decoded SELCAL burst.
type selcalDetection struct {
	Code     string
	Selcal32 bool
	OffsetHz float64
	Duration float64 // seconds from start of pulse 1 to end of pulse 2

	// MarginDB is how far the weaker tone stood above the in-band noise floor,
	// taken from the worse of the two pulses.  It measures how comfortably the
	// burst cleared the detection thresholds, and carries the FFT's processing
	// gain, so it reads well above the audio SNR and is not comparable with the
	// receiver's calibrated signal level.  Reported as decode confidence only —
	// the signal level quoted for a call comes from the receiver instead.
	MarginDB float64

	// StartSec and EndSec bracket the burst on the detector's sample clock, so
	// the receiver's own signal measurements can be looked up for that window.
	StartSec float64
	EndSec   float64
}

// selcalDetector consumes a mono audio stream and emits decoded bursts.
// It is not safe for concurrent use; each channel owns one.
type selcalDetector struct {
	rate    int
	n       int // FFT size
	hop     int
	binHz   float64
	loBin   int
	hiBin   int
	window  []float64
	plan    *fftPlan
	scratch []complex128
	mags    []float64
	sorted  []float64

	buf      []float64 // input accumulator, always < n+hop samples
	consumed int64     // samples retired from buf, for frame timestamps

	cur  *toneRun
	prev *toneRun

	// Diagnostics.  diag accumulates frame rejection reasons; onEvent, when set,
	// receives human-readable notes about runs that did not become a code.
	diag    *selcalDiag
	onEvent func(string)

	// lastLevelDB and lastNoiseDB expose the most recent frame's in-band peak
	// and noise-floor levels for the UI signal meter.
	lastLevelDB float64
	lastNoiseDB float64
}

// newSelcalDetector builds a detector for the given sample rate.  The FFT size
// is the smallest power of two spanning at least 0.30 s, giving roughly 3 Hz
// bins at 12 kHz — comfortably finer than the 16.6 Hz minimum tone spacing,
// while still fitting several whole windows inside the shortest legal pulse.
func newSelcalDetector(rate int) *selcalDetector {
	n := 1
	for float64(n) < 0.30*float64(rate) {
		n <<= 1
	}
	d := &selcalDetector{
		rate:  rate,
		n:     n,
		hop:   n / 4,
		binHz: float64(rate) / float64(n),
		plan:  newFFTPlan(n),
	}
	d.loBin = int(selcalBandLowHz / d.binHz)
	d.hiBin = int(selcalBandHighHz/d.binHz) + 1
	if d.hiBin > n/2-2 {
		d.hiBin = n/2 - 2
	}
	if d.loBin < 1 {
		d.loBin = 1
	}
	d.window = make([]float64, n)
	for i := range d.window {
		d.window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	d.scratch = make([]complex128, n)
	d.mags = make([]float64, d.hiBin-d.loBin+1)
	d.sorted = make([]float64, len(d.mags))
	d.buf = make([]float64, 0, n+d.hop)
	d.diag = newSelcalDiag()
	return d
}

// windowSec is the analysis window length in seconds.
func (d *selcalDetector) windowSec() float64 { return float64(d.n) / float64(d.rate) }

// feed appends samples (normalised to roughly ±1.0) and returns any bursts
// that completed within this batch.
func (d *selcalDetector) feed(samples []float64) []selcalDetection {
	var out []selcalDetection
	d.buf = append(d.buf, samples...)

	for len(d.buf) >= d.n {
		frameSec := float64(d.consumed) / float64(d.rate)
		if det := d.processFrame(d.buf[:d.n], frameSec); det != nil {
			out = append(out, *det)
		}
		d.buf = d.buf[d.hop:]
		d.consumed += int64(d.hop)
	}

	// Keep the accumulator from creeping forward in memory forever.
	if cap(d.buf) > 4*(d.n+d.hop) {
		nb := make([]float64, len(d.buf), d.n+d.hop)
		copy(nb, d.buf)
		d.buf = nb
	}
	return out
}

// processFrame analyses one window and advances the run state machine.
func (d *selcalDetector) processFrame(frame []float64, frameSec float64) *selcalDetection {
	for i := 0; i < d.n; i++ {
		d.scratch[i] = complex(frame[i]*d.window[i], 0)
	}
	d.plan.transform(d.scratch)

	for i := d.loBin; i <= d.hiBin; i++ {
		re, im := real(d.scratch[i]), imag(d.scratch[i])
		d.mags[i-d.loBin] = math.Sqrt(re*re + im*im)
	}

	// In-band noise floor: the median is robust to the handful of bins that a
	// genuine tone pair occupies.
	copy(d.sorted, d.mags)
	sort.Float64s(d.sorted)
	noise := d.sorted[len(d.sorted)/2]
	noiseDB := ampDB(noise)
	d.lastNoiseDB = noiseDB
	d.lastLevelDB = ampDB(d.sorted[len(d.sorted)-1])

	pair, offset, snr, reason := d.resolvePair(noiseDB)
	d.diag.note(reason)
	if reason != rejNone {
		// This is the usual way a burst completes: the second pulse ends and
		// the next frame resolves nothing, so closing the run here is what
		// yields the code.
		det := d.closeRun(frameSec)
		d.expire(frameSec)
		return det
	}

	if d.cur != nil && d.cur.pair == pair {
		// Extend the current run, accumulating means incrementally.
		f := float64(d.cur.frames)
		d.cur.offsetHz = (d.cur.offsetHz*f + offset) / (f + 1)
		d.cur.snrDB = (d.cur.snrDB*f + snr) / (f + 1)
		d.cur.frames++
		d.cur.endSec = frameSec
		return nil
	}

	// A different pair — retire whatever was in progress, then start fresh.
	det := d.closeRun(frameSec)
	d.cur = &toneRun{
		pair:     pair,
		startSec: frameSec,
		endSec:   frameSec,
		frames:   1,
		offsetHz: offset,
		snrDB:    snr,
	}
	return det
}

// resolvePair extracts the dominant tone pair from the current magnitude
// spectrum, or reports ok=false when the frame does not look like a SELCAL
// pulse.  The returned pair is sorted into designator order.
func (d *selcalDetector) resolvePair(noiseDB float64) (pair [2]int, offsetHz, snrDB float64, reason string) {
	type peak struct {
		hz float64
		db float64
	}
	var peaks []peak

	for i := 1; i < len(d.mags)-1; i++ {
		if d.mags[i] <= d.mags[i-1] || d.mags[i] < d.mags[i+1] {
			continue
		}
		// Parabolic interpolation on log magnitude recovers the true peak
		// frequency to a fraction of a bin.
		l0, l1, l2 := ampDB(d.mags[i-1]), ampDB(d.mags[i]), ampDB(d.mags[i+1])
		denom := l0 - 2*l1 + l2
		delta := 0.0
		if denom != 0 {
			delta = 0.5 * (l0 - l2) / denom
		}
		if delta > 1 || delta < -1 {
			delta = 0
		}
		peaks = append(peaks, peak{
			hz: (float64(d.loBin+i) + delta) * d.binHz,
			db: l1,
		})
	}
	if len(peaks) < 2 {
		return pair, 0, 0, rejFewPeaks
	}
	sort.Slice(peaks, func(a, b int) bool { return peaks[a].db > peaks[b].db })

	p1, p2 := peaks[0], peaks[1]
	if p2.db-noiseDB < selcalPeakOverNoiseDB {
		return pair, 0, 0, rejWeak
	}

	// From here the frame plausibly contains a tone pair, so record how close
	// each threshold came to rejecting it.
	if imb := p1.db - p2.db; imb > d.diag.maxImbalanceDB {
		d.diag.maxImbalanceDB = imb
	}
	if len(peaks) >= 3 {
		if third := p2.db - peaks[2].db; third < d.diag.minThirdDB {
			d.diag.minThirdDB = third
		}
	}

	if p1.db-p2.db > selcalPairBalanceDB {
		return pair, 0, 0, rejUnbalanced
	}
	if len(peaks) >= 3 && p2.db-peaks[2].db < selcalThirdPeakDB {
		return pair, 0, 0, rejThirdPeak
	}

	i1, off1, ok1 := nearestTone(p1.hz)
	i2, off2, ok2 := nearestTone(p2.hz)
	if !ok1 || !ok2 {
		// Record how far off the grid the peak fell, which is what shows a
		// station sitting off frequency.
		for _, p := range []peak{p1, p2} {
			if _, off, ok := nearestToneUnbounded(p.hz); !ok {
				_ = off
			} else if a := math.Abs(off); a > d.diag.maxOffsetHz {
				d.diag.maxOffsetHz = a
			}
		}
		return pair, 0, 0, rejOffGrid
	}
	if i1 == i2 {
		return pair, 0, 0, rejSameTone
	}
	for _, off := range []float64{off1, off2} {
		if a := math.Abs(off); a > d.diag.maxOffsetHz {
			d.diag.maxOffsetHz = a
		}
	}
	if math.Abs(off1-off2) > selcalPairOffsetHz {
		return pair, 0, 0, rejOffsetPair
	}

	if i1 > i2 {
		i1, i2 = i2, i1
	}
	return [2]int{i1, i2}, (off1 + off2) / 2, p2.db - noiseDB, rejNone
}

// nearestToneUnbounded is nearestTone without the offset limit, used only to
// report how far a peak sat from the nearest tone.
func nearestToneUnbounded(hz float64) (idx int, offsetHz float64, ok bool) {
	best, bestErr := -1, math.MaxFloat64
	for i, t := range selcalToneTable {
		if e := math.Abs(hz - t.Hz); e < bestErr {
			best, bestErr = i, e
		}
	}
	if best < 0 {
		return 0, 0, false
	}
	return best, hz - selcalToneTable[best].Hz, true
}

// closeRun retires the in-progress run.  If it is long enough to be a real
// pulse it becomes the pending first pulse, or completes a code together with
// an already-pending one.
func (d *selcalDetector) closeRun(nowSec float64) *selcalDetection {
	run := d.cur
	d.cur = nil
	if run == nil {
		return nil
	}
	dur := run.endSec - run.startSec
	if run.frames < selcalMinFrames || dur < selcalMinRunSec || dur > selcalMaxRunSec {
		if run.frames >= 2 {
			d.report(fmt.Sprintf("discarded a %s pulse lasting %.2f s (%d frames): %s",
				runTones(run), dur, run.frames, runLenReason(run, dur)))
		}
		return nil
	}

	d.report(fmt.Sprintf("pulse %s  %.2f s  margin %.1f dB  offset %+.1f Hz  [%s]",
		runTones(run), dur, run.snrDB, run.offsetHz, d.diag.summary()))
	d.diag.reset()

	if d.prev != nil {
		gap := run.startSec - d.prev.endSec
		span := run.endSec - d.prev.startSec + d.windowSec()
		why := pairFailure(d.prev, run, gap, span)
		if why != "" {
			d.report(fmt.Sprintf("did not pair %s with %s: %s",
				runTones(d.prev), runTones(run), why))
		}
		if why == "" {
			if code := selcalCode(d.prev.pair, run.pair); code != "" {
				det := &selcalDetection{
					Code:     code,
					Selcal32: isSelcal32(d.prev.pair, run.pair),
					MarginDB: math.Min(d.prev.snrDB, run.snrDB),
					OffsetHz: (d.prev.offsetHz + run.offsetHz) / 2,
					Duration: span,
					StartSec: d.prev.startSec,
					EndSec:   run.endSec + d.windowSec(),
				}
				// A repeated call is pulse1,pulse2,pause,pulse1,pulse2 — drop
				// the pending state so pulse 2 cannot also start a new code.
				d.prev = nil
				return det
			}
		}
	}

	d.prev = run
	return nil
}

// runTones renders a run's tone pair, e.g. "AB".
func runTones(r *toneRun) string {
	a, b := r.pair[0], r.pair[1]
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%c%c", selcalToneTable[a].Char, selcalToneTable[b].Char)
}

// runLenReason explains why a run was too short or too long to be a pulse.
func runLenReason(run *toneRun, dur float64) string {
	switch {
	case run.frames < selcalMinFrames:
		return fmt.Sprintf("needs at least %d frames — the tones probably broke up mid-pulse",
			selcalMinFrames)
	case dur < selcalMinRunSec:
		return fmt.Sprintf("shorter than %.2f s", selcalMinRunSec)
	default:
		return fmt.Sprintf("longer than %.2f s, so not a SELCAL pulse", selcalMaxRunSec)
	}
}

// pairFailure explains why two pulses were not accepted as one code, or returns
// "" if they were.
func pairFailure(prev, run *toneRun, gap, span float64) string {
	switch {
	case gap > selcalMaxGapSec:
		return fmt.Sprintf("gap of %.2f s exceeds %.2f s", gap, selcalMaxGapSec)
	case span < selcalMinSpanSec:
		return fmt.Sprintf("total span %.2f s is under %.2f s", span, selcalMinSpanSec)
	case span > selcalMaxSpanSec:
		return fmt.Sprintf("total span %.2f s exceeds %.2f s", span, selcalMaxSpanSec)
	case math.Abs(prev.offsetHz-run.offsetHz) > selcalCodeOffsetHz:
		return fmt.Sprintf("the pulses disagree on frequency offset by %.1f Hz (limit %.0f)",
			math.Abs(prev.offsetHz-run.offsetHz), selcalCodeOffsetHz)
	case selcalCode(prev.pair, run.pair) == "":
		return "a tone repeats between the two pulses, which no SELCAL code does"
	}
	return ""
}

// report forwards a diagnostic note when debugging is enabled.
func (d *selcalDetector) report(msg string) {
	if d.onEvent != nil {
		d.onEvent(msg)
	}
}

// expire discards a pending first pulse once no second pulse can still follow
// it within the permitted gap.
func (d *selcalDetector) expire(nowSec float64) {
	if d.prev != nil && nowSec-d.prev.endSec > selcalMaxGapSec {
		d.prev = nil
	}
}

// nearestTone maps a measured frequency to the closest tabulated tone,
// rejecting it when the offset exceeds what a plausible frequency error could
// explain.  The tolerance is deliberately kept below half the minimum tone
// spacing so the mapping can never be ambiguous.
func nearestTone(hz float64) (idx int, offsetHz float64, ok bool) {
	best, bestErr := -1, math.MaxFloat64
	for i, t := range selcalToneTable {
		if e := math.Abs(hz - t.Hz); e < bestErr {
			best, bestErr = i, e
		}
	}
	if best < 0 || bestErr > selcalMaxOffsetHz {
		return 0, 0, false
	}
	return best, hz - selcalToneTable[best].Hz, true
}

func ampDB(a float64) float64 {
	if a <= 1e-12 {
		return -240
	}
	return 20 * math.Log10(a)
}

// ---------------------------------------------------------------------------
// Channel configuration
// ---------------------------------------------------------------------------

// selcalChannelCfg is one configured HF voice channel.
type selcalChannelCfg struct {
	FreqKHz float64
	Label   string
	Decode  bool // false = relay audio only (e.g. a VOLMET broadcast)
}

// FreqHz returns the tuned carrier frequency.  Aeronautical HF voice channels
// are quoted as the USB carrier frequency, which is exactly what the receiver
// wants, so SELCAL tones land at their tabulated audio frequencies.
func (c selcalChannelCfg) FreqHz() int { return int(math.Round(c.FreqKHz * 1000)) }

// Name is the display label, falling back to the frequency.
func (c selcalChannelCfg) Name() string {
	if c.Label != "" {
		return c.Label
	}
	return c.ID()
}

// ID is the stable key used in URLs and JSON.
func (c selcalChannelCfg) ID() string {
	return strconv.FormatFloat(c.FreqKHz, 'f', -1, 64)
}

// parseSelcalFreqs parses the -selcal-freqs value.
//
// Format: comma-separated  kHz[:label[:audio]]
//
//	"5598:NAT-A, 8906:NAT-A, 5505:Shannon Volmet:audio"
//
// A third field of "audio" relays the channel to listeners without running the
// SELCAL detector on it — useful for continuous broadcasts that never carry
// SELCAL and would only clutter the log.
func parseSelcalFreqs(s string) ([]selcalChannelCfg, error) {
	var out []selcalChannelCfg
	seen := map[string]bool{}

	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ":")
		khz, err := strconv.ParseFloat(strings.TrimSpace(fields[0]), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid frequency %q (expected kHz, e.g. 8906)", fields[0])
		}
		if khz < 2000 || khz > 30000 {
			return nil, fmt.Errorf("frequency %g kHz is outside the HF range (2000–30000 kHz)", khz)
		}
		cfg := selcalChannelCfg{FreqKHz: khz, Decode: true}
		if len(fields) > 1 {
			cfg.Label = strings.TrimSpace(fields[1])
		}
		if len(fields) > 2 && strings.EqualFold(strings.TrimSpace(fields[2]), "audio") {
			cfg.Decode = false
		}
		if seen[cfg.ID()] {
			return nil, fmt.Errorf("duplicate frequency %g kHz", khz)
		}
		seen[cfg.ID()] = true
		out = append(out, cfg)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].FreqKHz < out[j].FreqKHz })
	return out, nil
}
