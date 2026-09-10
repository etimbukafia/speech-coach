// pitch/yin.go
//
// YIN pitch detection algorithm (Cheveigné & Kawahara, 2002).
//
// YIN estimates the fundamental frequency (F0) of a periodic signal by
// finding the lag at which the waveform best repeats itself. It improves
// on basic autocorrelation with cumulative mean normalization (removes
// the bias toward lag=0) and an absolute threshold search (avoids octave
// errors by picking the first good dip, not the global minimum).
//
// This is a pure-Go implementation with no external dependencies.
// All internal buffers are pre-allocated so the hot path is zero-alloc.

package pitch

import (
	"fmt"
	"math"
)

// Result holds the output of one pitch detection pass.
type Result struct {
	Frequency  float64 // Detected fundamental frequency in Hz (0 if unvoiced)
	Confidence float64 // 0.0–1.0: how periodic the signal is (1 = pure tone)
	Note       string  // Musical note name, e.g. "A4" (empty if unvoiced)
	MIDINote   int     // MIDI note number 0–127 (-1 if unvoiced)
	CentsOff   int     // Cents sharp (+) or flat (-) from the nearest note
}

// Detector runs the YIN algorithm on PCM16 audio frames.
//
// Create one with NewDetector and reuse it across frames — it pre-allocates
// all working memory so the per-frame Detect call does not allocate.
type Detector struct {
	sampleRate int
	minLag     int     // sampleRate / maxFreqHz
	maxLag     int     // sampleRate / minFreqHz
	threshold  float64 // CMND threshold for pitch acceptance (lower = stricter)

	// Pre-allocated working buffers (reused every Detect call)
	samples []float32 // PCM16 → float32 conversion
	diff    []float64 // difference function d(τ)
	cmnd    []float64 // cumulative mean normalized difference d'(τ)
}

// NewDetector creates a YIN pitch detector tuned for the given frequency range.
//
// Recommended parameters for singing voice:
//
//	sampleRate = 16000
//	minFreqHz  = 75.0   (covers bass voices down to ~D2)
//	maxFreqHz  = 800.0  (covers soprano up to ~G5; higher needs larger windows)
//	threshold  = 0.15   (0.10 = strict/fewer detections, 0.20 = relaxed/more)
func NewDetector(sampleRate int, minFreqHz, maxFreqHz, threshold float64) *Detector {
	minLag := int(float64(sampleRate) / maxFreqHz)
	maxLag := int(float64(sampleRate) / minFreqHz)

	if minLag < 1 {
		minLag = 1
	}

	return &Detector{
		sampleRate: sampleRate,
		minLag:     minLag,
		maxLag:     maxLag,
		threshold:  threshold,
		diff:       make([]float64, maxLag+1),
		cmnd:       make([]float64, maxLag+1),
	}
}

// Detect runs YIN on raw PCM16 little-endian audio bytes.
//
// For best results, pass at least 40ms of audio (640 bytes at 16kHz).
// Shorter windows reduce accuracy for low-pitched voices.
func (d *Detector) Detect(pcm16 []byte) Result {
	if len(pcm16) < 4 || len(pcm16)%2 != 0 {
		return Result{MIDINote: -1}
	}

	// Convert PCM16 bytes → float32 samples in [-1.0, 1.0].
	// Reuses d.samples to avoid per-frame allocation.
	n := len(pcm16) / 2
	if cap(d.samples) < n {
		d.samples = make([]float32, n)
	}
	d.samples = d.samples[:n]

	for i := 0; i < len(pcm16); i += 2 {
		sample := int16(pcm16[i]) | int16(pcm16[i+1])<<8
		d.samples[i/2] = float32(sample) / 32768.0
	}

	return d.detectFloat(d.samples)
}

// detectFloat is the core YIN algorithm operating on float32 samples.
func (d *Detector) detectFloat(samples []float32) Result {
	n := len(samples)

	// The max lag we can test is limited by the window size.
	// We need at least W comparison points where W = n - maxLag.
	// A reasonable minimum W is maxLag itself (so we need n >= 2*maxLag).
	maxLag := d.maxLag
	if maxLag >= n/2 {
		maxLag = n/2 - 1
	}
	if maxLag <= d.minLag {
		return Result{MIDINote: -1}
	}

	W := n - maxLag // number of summation points

	// ── Step 1: Difference function ─────────────────────────────────
	//
	//   d(τ) = Σ_{i=0}^{W-1} (x[i] - x[i+τ])²
	//
	// For each candidate lag τ, we measure how different the signal is
	// from a shifted copy of itself. When τ matches the true period,
	// d(τ) drops toward zero because the waveform is repeating.
	for tau := 0; tau <= maxLag; tau++ {
		var sum float64
		for i := 0; i < W; i++ {
			delta := float64(samples[i]) - float64(samples[i+tau])
			sum += delta * delta
		}
		d.diff[tau] = sum
	}

	// ── Step 2: Cumulative mean normalized difference (CMND) ────────
	//
	//   d'(0) = 1
	//   d'(τ) = d(τ) / [ (1/τ) × Σ_{j=1}^{τ} d(j) ]
	//
	// Raw d(τ) is biased: it naturally decreases for small τ because
	// nearby samples are always similar. CMND fixes this by dividing
	// each value by the running average, making the dips at true
	// periods stand out clearly against the baseline of ~1.0.
	d.cmnd[0] = 1.0
	var runningSum float64
	for tau := 1; tau <= maxLag; tau++ {
		runningSum += d.diff[tau]
		if runningSum < 1e-10 {
			// Silence or DC — avoid division by zero.
			d.cmnd[tau] = 1.0
		} else {
			d.cmnd[tau] = d.diff[tau] * float64(tau) / runningSum
		}
	}

	// ── Step 3: Absolute threshold ──────────────────────────────────
	//
	// Scan from the smallest lag (highest frequency) upward. Accept the
	// FIRST dip below the threshold instead of the global minimum.
	//
	// Why first, not deepest? Because the deepest dip is often at 2×
	// the true period (one octave lower). By taking the first good dip,
	// YIN avoids the "octave error" that plagues naive autocorrelation.
	bestTau := -1
	for tau := d.minLag; tau <= maxLag; tau++ {
		if d.cmnd[tau] < d.threshold {
			// Walk forward to find the local minimum within this dip.
			for tau+1 <= maxLag && d.cmnd[tau+1] < d.cmnd[tau] {
				tau++
			}
			bestTau = tau
			break
		}
	}

	if bestTau < 0 {
		// No lag passed the threshold — the frame is likely unvoiced.
		return Result{MIDINote: -1}
	}

	// ── Step 4: Parabolic interpolation ─────────────────────────────
	//
	// The true minimum usually falls between integer sample lags.
	// Fitting a parabola through the three points around bestTau gives
	// sub-sample precision, which improves frequency accuracy by ~1 Hz.
	refinedTau := float64(bestTau)
	if bestTau > 0 && bestTau < maxLag {
		alpha := d.cmnd[bestTau-1]
		beta := d.cmnd[bestTau]
		gamma := d.cmnd[bestTau+1]

		denom := alpha - 2*beta + gamma
		if math.Abs(denom) > 1e-10 {
			refinedTau += 0.5 * (alpha - gamma) / denom
		}
	}

	// ── Step 5: Lag → frequency ─────────────────────────────────────
	freq := float64(d.sampleRate) / refinedTau

	// Confidence = 1 - d'(bestTau). A pure sine wave gives d'≈0 → confidence≈1.
	confidence := 1.0 - d.cmnd[bestTau]
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	// ── Step 6: Frequency → musical note ────────────────────────────
	note, midi, cents := FreqToNote(freq)

	return Result{
		Frequency:  freq,
		Confidence: confidence,
		Note:       note,
		MIDINote:   midi,
		CentsOff:   cents,
	}
}

// noteNames maps the 12 chromatic pitch classes to their standard names.
var noteNames = [12]string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}

// FreqToNote converts a frequency in Hz to the nearest musical note.
//
// Returns the note name (e.g. "A4"), MIDI note number (0–127), and
// how many cents sharp (+) or flat (-) the frequency is from that note.
//
// Reference: A4 = 440 Hz = MIDI 69.
func FreqToNote(freq float64) (note string, midi int, cents int) {
	if freq <= 0 {
		return "", -1, 0
	}

	// MIDI note number (continuous): 69 + 12 × log₂(freq / 440)
	midiFloat := 69.0 + 12.0*math.Log2(freq/440.0)
	midi = int(math.Round(midiFloat))
	cents = int(math.Round((midiFloat - float64(midi)) * 100))

	if midi < 0 || midi > 127 {
		return "", -1, 0
	}

	noteName := noteNames[midi%12]
	octave := (midi / 12) - 1
	note = fmt.Sprintf("%s%d", noteName, octave)

	return note, midi, cents
}
