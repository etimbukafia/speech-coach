// pitch/classify.go
//
// Voice type classification from a collection of pitch detections.
// Uses the median pitch and comfortable range (10th–90th percentile)
// to estimate the singer's voice type.

package pitch

import (
	"fmt"
	"sort"
)

// VoiceType is the estimated vocal classification.
type VoiceType struct {
	Name       string  // e.g. "Baritone"
	MedianNote string  // The note at the center of their singing
	MedianFreq float64 // Frequency of the median pitch
	MedianMIDI int     // MIDI note of the median pitch
	LowNote    string  // 10th percentile note (comfortable low)
	HighNote   string  // 90th percentile note (comfortable high)
	LowMIDI    int
	HighMIDI   int
}

// voiceRange defines one row of the voice type lookup table.
type voiceRange struct {
	name    string
	minMIDI int // inclusive
	maxMIDI int // exclusive
}

// Voice type classification table, ordered from lowest to highest.
// Based on where the median pitch falls.
//
//	Bass:           E2 (40) – E3 (52)
//	Baritone:       E3 (52) – A3 (57)
//	Tenor:          A3 (57) – D4 (62)
//	Alto:           D4 (62) – G4 (67)
//	Mezzo-soprano:  G4 (67) – C5 (72)
//	Soprano:        C5 (72) +
var voiceTypes = []voiceRange{
	{"Bass", 0, 52},
	{"Baritone", 52, 57},
	{"Tenor", 57, 62},
	{"Alto", 62, 67},
	{"Mezzo-Soprano", 67, 72},
	{"Soprano", 72, 128},
}

// ClassifyVoice estimates the singer's voice type from their detected pitches.
//
// It works by:
//  1. Sorting all pitch detections by MIDI note
//  2. Taking the median (where most of the singing happens)
//  3. Computing the 10th and 90th percentiles (comfortable range,
//     filtering out warm-up squeaks and strain notes)
//  4. Looking up the voice type from the median
//
// Returns a zero VoiceType if there are no pitches to analyze.
func ClassifyVoice(pitches []Result) VoiceType {
	if len(pitches) == 0 {
		return VoiceType{}
	}

	// Sort a copy by MIDI note number.
	sorted := make([]Result, len(pitches))
	copy(sorted, pitches)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].MIDINote < sorted[j].MIDINote
	})

	// Percentile indices.
	median := sorted[len(sorted)/2]
	p10 := sorted[percentileIndex(len(sorted), 10)]
	p90 := sorted[percentileIndex(len(sorted), 90)]

	// Classify by median MIDI note.
	name := "Unknown"
	for _, vr := range voiceTypes {
		if median.MIDINote >= vr.minMIDI && median.MIDINote < vr.maxMIDI {
			name = vr.name
			break
		}
	}

	return VoiceType{
		Name:       name,
		MedianNote: median.Note,
		MedianFreq: median.Frequency,
		MedianMIDI: median.MIDINote,
		LowNote:    p10.Note,
		HighNote:   p90.Note,
		LowMIDI:    p10.MIDINote,
		HighMIDI:   p90.MIDINote,
	}
}

// percentileIndex returns the slice index for the given percentile (0–100).
func percentileIndex(length, percentile int) int {
	idx := (length - 1) * percentile / 100
	if idx < 0 {
		return 0
	}
	if idx >= length {
		return length - 1
	}
	return idx
}

// PitchReport generates a concise summary of pitch data for one speaking turn.
// This is designed to be injected into an LLM prompt so the coach can comment
// on the user's voice characteristics.
//
// No threshold is applied — the LLM decides what's "deep enough" based on
// the raw data and its coaching persona.
//
// Example output:
//
//	"Pitch: median 185 Hz (F#3, Baritone). Comfortable range: E3–A3."
func PitchReport(pitches []Result) string {
	if len(pitches) == 0 {
		return "Pitch: no pitch detected (too quiet or too short)."
	}

	voice := ClassifyVoice(pitches)

	report := fmt.Sprintf(
		"Pitch: median %.0f Hz (%s, %s). Comfortable range: %s–%s.",
		voice.MedianFreq, voice.MedianNote, voice.Name, voice.LowNote, voice.HighNote,
	)

	// Track drift: first vs last pitch.
	first := pitches[0]
	last := pitches[len(pitches)-1]
	drift := last.Frequency - first.Frequency
	if drift > 20 {
		report += fmt.Sprintf(" Voice drifted UP from %.0f Hz to %.0f Hz.", first.Frequency, last.Frequency)
	} else if drift < -20 {
		report += fmt.Sprintf(" Voice dropped from %.0f Hz to %.0f Hz.", first.Frequency, last.Frequency)
	}

	return report
}
