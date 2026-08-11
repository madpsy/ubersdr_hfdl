package main

// Detector tolerance against the impairments real HF signals arrive with.
// These lock in the headroom the thresholds were chosen to give: SELCAL is
// transmitted with the two tones within 3 dB of each other, but selective
// fading routinely pulls them apart by 10 dB or more over a path, and a call
// that is plainly audible must still decode.

import (
	"math"
	"math/rand"
	"testing"
)

type burstOpts struct {
	imbalanceDB  float64 // level difference between the two tones of a pulse (selective fading)
	pulseSec     float64
	gapSec       float64
	noise        float64
	offsetHz     float64
	interfererDB float64 // steady third tone, relative to the SELCAL tones
	interfHz     float64
	fadeDepthDB  float64 // slow fade across the burst
	fadeHz       float64
}

func synth(t *testing.T, o burstOpts) []float64 {
	rnd := rand.New(rand.NewSource(42))
	amp := 0.15
	a2 := amp * math.Pow(10, -o.imbalanceDB/20)
	interf := 0.0
	if o.interfererDB != 0 {
		interf = amp * math.Pow(10, o.interfererDB/20)
	}

	var out []float64
	add := func(secs float64, f1, f2 float64, tones bool) {
		base := len(out)
		for i := 0; i < int(secs*testRate); i++ {
			ts := float64(base+i) / testRate
			v := rnd.NormFloat64() * o.noise
			if tones {
				g := 1.0
				if o.fadeDepthDB > 0 {
					// Slow amplitude fade, as HF signals routinely show.
					g = math.Pow(10, -o.fadeDepthDB/20*0.5*(1-math.Cos(2*math.Pi*o.fadeHz*ts)))
				}
				v += g * (amp*math.Sin(2*math.Pi*(f1+o.offsetHz)*ts) +
					a2*math.Sin(2*math.Pi*(f2+o.offsetHz)*ts))
			}
			if interf > 0 {
				v += interf * math.Sin(2*math.Pi*o.interfHz*ts)
			}
			out = append(out, v)
		}
	}
	add(0.6, 0, 0, false)
	add(o.pulseSec, toneHz(t, 'A'), toneHz(t, 'B'), true)
	add(o.gapSec, 0, 0, false)
	add(o.pulseSec, toneHz(t, 'C'), toneHz(t, 'D'), true)
	add(0.7, 0, 0, false)
	return out
}

func decodesBurst(t *testing.T, o burstOpts) bool {
	for _, d := range decodeAll(synth(t, o)) {
		if d.Code == "AB-CD" {
			return true
		}
	}
	return false
}

// baseBurst is a clean, nominal transmission.
func baseBurst() burstOpts {
	return burstOpts{pulseSec: 1.0, gapSec: 0.2, noise: 0.002, interfHz: 1000}
}

func TestToleratesSelectiveFading(t *testing.T) {
	// The two tones of a pulse can be over a kilohertz apart, so an HF path
	// fades them independently.  Requiring them to arrive at similar levels was
	// the detector's tightest constraint and the likeliest cause of a missed
	// call.
	for _, imbalance := range []float64{0, 4, 8, 12, 15} {
		o := baseBurst()
		o.imbalanceDB = imbalance
		if !decodesBurst(t, o) {
			t.Errorf("missed a burst with %.0f dB between its two tones — "+
				"routine for HF selective fading", imbalance)
		}
	}
}

func TestToleratesNoiseAndFading(t *testing.T) {
	for _, noise := range []float64{0.002, 0.02, 0.06, 0.12} {
		o := baseBurst()
		o.noise = noise
		if !decodesBurst(t, o) {
			t.Errorf("missed a burst at noise level %.3f", noise)
		}
	}
	// A slow fade across the burst moves both tones together, so it should not
	// matter however deep it gets.
	for _, depth := range []float64{6, 20, 30} {
		o := baseBurst()
		o.fadeDepthDB, o.fadeHz = depth, 1.0
		if !decodesBurst(t, o) {
			t.Errorf("missed a burst fading %.0f dB during the transmission", depth)
		}
	}
}

func TestToleratesTimingAcrossTheAnnex10Range(t *testing.T) {
	// Annex 10 allows pulses of 1.0 +/- 0.25 s and gaps of 0.2 +/- 0.1 s.  The
	// bounds here run past both ends, since a fading edge shortens the pulse
	// the detector actually sees.
	for _, pulse := range []float64{0.7, 0.75, 1.0, 1.25, 1.4} {
		o := baseBurst()
		o.pulseSec = pulse
		if !decodesBurst(t, o) {
			t.Errorf("missed a burst with %.2f s pulses", pulse)
		}
	}
	for _, gap := range []float64{0.05, 0.1, 0.2, 0.3, 0.4} {
		o := baseBurst()
		o.gapSec = gap
		if !decodesBurst(t, o) {
			t.Errorf("missed a burst with a %.2f s gap", gap)
		}
	}
}

func TestToleratesModerateInterference(t *testing.T) {
	// A steady carrier sharing the channel must not blind the detector until it
	// approaches the level of the tones themselves.
	for _, rel := range []float64{-30, -20, -12, -8} {
		o := baseBurst()
		o.interfererDB = rel
		if !decodesBurst(t, o) {
			t.Errorf("missed a burst with an interferer %.0f dB below the tones", rel)
		}
	}
}

func TestRejectsBurstFarOffFrequency(t *testing.T) {
	// Beyond half the minimum tone spacing the mapping would be ambiguous, so a
	// badly off-frequency burst must be dropped rather than decoded as some
	// other code.
	o := baseBurst()
	o.offsetHz = 12
	if decodesBurst(t, o) {
		t.Error("decoded a burst 12 Hz off frequency, where tone identification is ambiguous")
	}
}
