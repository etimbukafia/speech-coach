package pitch

import (
	"math"
	"testing"
)

// generateSineWave builds a PCM16 little-endian byte slice containing a pure
// sine wave at the given frequency. This is the simplest possible test signal
// for a pitch detector: one frequency, no harmonics, no noise.
func generateSineWave(freq float64, sampleRate int, durationSamples int) []byte {
	out := make([]byte, durationSamples*2) // 2 bytes per sample (PCM16)
	for i := 0; i < durationSamples; i++ {
		// Generate a sine sample in [-1.0, 1.0], scale to int16 range.
		t := float64(i) / float64(sampleRate)
		sample := math.Sin(2 * math.Pi * freq * t)
		pcm := int16(sample * 32000) // leave a bit of headroom below 32767

		// Little-endian: low byte first, high byte second.
		out[i*2] = byte(pcm)
		out[i*2+1] = byte(pcm >> 8)
	}
	return out
}

// TestYINDetectsSineWaves verifies that YIN correctly estimates the fundamental
// frequency of pure sine waves across the typical singing vocal range.
func TestYINDetectsSineWaves(t *testing.T) {
	const sampleRate = 16000

	tests := []struct {
		name     string
		freq     float64 // Hz
		wantNote string
	}{
		{"A2 (low male)", 110.0, "A2"},
		{"A3", 220.0, "A3"},
		{"C4 (middle C)", 261.63, "C4"},
		{"A4 (concert pitch)", 440.0, "A4"},
		{"C5 (soprano)", 523.25, "C5"},
	}

	// 40ms window = 640 samples (2 frames worth) — gives enough
	// signal for YIN to work accurately even at low frequencies.
	const windowSamples = 640

	detector := NewDetector(sampleRate, 75.0, 800.0, 0.15)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pcm := generateSineWave(tt.freq, sampleRate, windowSamples)
			result := detector.Detect(pcm)

			if result.MIDINote == -1 {
				t.Fatalf("YIN returned unvoiced for %.1f Hz sine wave", tt.freq)
			}
			if result.Note != tt.wantNote {
				t.Errorf("note = %q, want %q (detected %.2f Hz)", result.Note, tt.wantNote, result.Frequency)
			}

			// Frequency should be within ±2 Hz of the true value.
			if math.Abs(result.Frequency-tt.freq) > 2.0 {
				t.Errorf("frequency = %.2f Hz, want ≈%.2f Hz (off by %.2f Hz)", result.Frequency, tt.freq, math.Abs(result.Frequency-tt.freq))
			}

			// Confidence should be very high for a clean sine wave.
			if result.Confidence < 0.85 {
				t.Errorf("confidence = %.3f, want ≥ 0.85 for a pure sine", result.Confidence)
			}

			t.Logf("✓ %s: detected %.2f Hz (note=%s, confidence=%.3f, cents=%+d)",
				tt.name, result.Frequency, result.Note, result.Confidence, result.CentsOff)
		})
	}
}

// TestYINRejectsSilence verifies that YIN does not hallucinate pitch from silence.
func TestYINRejectsSilence(t *testing.T) {
	detector := NewDetector(16000, 75.0, 800.0, 0.15)

	silence := make([]byte, 1280) // 640 samples of zeros
	result := detector.Detect(silence)

	if result.MIDINote != -1 {
		t.Errorf("expected unvoiced for silence, got note=%s freq=%.2f", result.Note, result.Frequency)
	}
}

// TestFreqToNote verifies the frequency-to-musical-note conversion.
func TestFreqToNote(t *testing.T) {
	tests := []struct {
		freq     float64
		wantNote string
		wantMIDI int
	}{
		{440.0, "A4", 69},
		{261.63, "C4", 60},
		{329.63, "E4", 64},
		{110.0, "A2", 45},
		{880.0, "A5", 81},
	}

	for _, tt := range tests {
		note, midi, cents := FreqToNote(tt.freq)
		if note != tt.wantNote {
			t.Errorf("FreqToNote(%.2f): note = %q, want %q", tt.freq, note, tt.wantNote)
		}
		if midi != tt.wantMIDI {
			t.Errorf("FreqToNote(%.2f): midi = %d, want %d", tt.freq, midi, tt.wantMIDI)
		}
		// Known reference frequencies should be within ±5 cents of the exact note.
		if cents > 5 || cents < -5 {
			t.Errorf("FreqToNote(%.2f): cents = %d, want near 0", tt.freq, cents)
		}
	}
}
