// speech-coach/pipeline.go
//
// The VAD + pitch detection session loop and summary reporting.
// main.go wires the components and calls RunVADSession.

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/etimbukafia/real-time-voice-pipeline-go/audio"
	"github.com/etimbukafia/real-time-voice-pipeline-go/vad"

	"github.com/etimbukafia/speech-coach/pitch"
)

// voiceSegment records the timestamps and pitch data of one continuous block of detected voice.
type voiceSegment struct {
	start    time.Time
	end      time.Time
	duration time.Duration
	pitches  []pitch.Result // every pitch detected during this segment
}

// RunVADSession reads audio frames from the source, runs them through the
// VAD state tracker and pitch detector, prints real-time events, and returns
// the detected voice segments when the context is cancelled or the source closes.
func RunVADSession(ctx context.Context, source audio.AudioSource, tracker *vad.StateTracker, pitchDetector *pitch.Detector) ([]voiceSegment, uint64, error) {
	frames, err := source.Stream(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to start audio stream: %w", err)
	}

	fmt.Println("🎙️  Listening… sing or speak into your microphone (Ctrl-C to stop)")
	fmt.Println()

	var (
		segments       []voiceSegment
		currentStart   time.Time
		currentPitches []pitch.Result
		isSpeaking     bool
		frameCount     uint64
		lastDotPrinted time.Time

		// pitchBuf accumulates 2 frames (40ms / 640 samples) for more
		// accurate pitch detection, especially at lower frequencies.
		pitchBuf []byte
	)

	for frame := range frames {
		if ctx.Err() != nil {
			break
		}

		frameCount++

		event, _, err := tracker.Process(frame)
		if err != nil {
			fmt.Printf("VAD error: %v\n", err)
			continue
		}

		switch event {
		case vad.SpeechStart:
			isSpeaking = true
			currentStart = frame.Timestamp
			currentPitches = currentPitches[:0]
			pitchBuf = pitchBuf[:0]
			lastDotPrinted = time.Time{}
			fmt.Printf("🎤 Voice detected!  [%s]\n", frame.Timestamp.Format("15:04:05.000"))

		case vad.SpeechEnd:
			if isSpeaking {
				dur := frame.Timestamp.Sub(currentStart)
				seg := voiceSegment{
					start:    currentStart,
					end:      frame.Timestamp,
					duration: dur,
					pitches:  make([]pitch.Result, len(currentPitches)),
				}
				copy(seg.pitches, currentPitches)
				segments = append(segments, seg)

				fmt.Printf("🔇 Silence          [%s]  (segment lasted %s)\n",
					frame.Timestamp.Format("15:04:05.000"), dur.Round(time.Millisecond))
				isSpeaking = false
			}

		default:
			// Nothing to do when not speaking.
		}

		// Run pitch detection on every frame during speech.
		if isSpeaking {
			// Accumulate into a 2-frame buffer for better low-freq accuracy.
			pitchBuf = append(pitchBuf, frame.Data...)
			if len(pitchBuf) > 2*audio.FrameSize {
				pitchBuf = pitchBuf[len(pitchBuf)-2*audio.FrameSize:]
			}

			if len(pitchBuf) >= 2*audio.FrameSize {
				result := pitchDetector.Detect(pitchBuf)
				if result.MIDINote >= 0 && result.Confidence > 0.5 {
					currentPitches = append(currentPitches, result)

					// Print pitch info at a readable rate (~every 500ms).
					if time.Since(lastDotPrinted) > 500*time.Millisecond {
						fmt.Printf("  ♪ %s  (%.1f Hz, confidence %.0f%%)\n",
							result.Note, result.Frequency, result.Confidence*100)
						lastDotPrinted = time.Now()
					}
				}
			}
		}
	}

	// Close out the last segment if still speaking when we stopped.
	if isSpeaking {
		dur := time.Since(currentStart)
		seg := voiceSegment{
			start:    currentStart,
			end:      time.Now(),
			duration: dur,
			pitches:  make([]pitch.Result, len(currentPitches)),
		}
		copy(seg.pitches, currentPitches)
		segments = append(segments, seg)
	}

	return segments, frameCount, nil
}

// PrintSummary outputs the final voice activity and pitch analysis report.
func PrintSummary(segments []voiceSegment, frameCount uint64) {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("  Voice Analysis Summary")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("  Total frames processed : %d\n", frameCount)
	fmt.Printf("  Voice segments detected: %d\n", len(segments))

	if len(segments) == 0 {
		fmt.Println("  (no voice activity detected)")
		fmt.Println("═══════════════════════════════════════════════════════════")
		return
	}

	// Aggregate timing.
	var totalDuration time.Duration
	var longestDur time.Duration
	for _, seg := range segments {
		totalDuration += seg.duration
		if seg.duration > longestDur {
			longestDur = seg.duration
		}
	}

	fmt.Printf("  Total voice time       : %s\n", totalDuration.Round(time.Millisecond))
	fmt.Printf("  Longest segment        : %s\n", longestDur.Round(time.Millisecond))
	fmt.Printf("  Average segment        : %s\n", (totalDuration / time.Duration(len(segments))).Round(time.Millisecond))
	fmt.Println()

	// Aggregate pitch across all segments.
	var allPitches []pitch.Result
	for _, seg := range segments {
		allPitches = append(allPitches, seg.pitches...)
	}

	if len(allPitches) > 0 {
		lowest, highest := pitchRange(allPitches)
		voice := pitch.ClassifyVoice(allPitches)

		fmt.Println("  ── Vocal Range ──────────────────────────────────────")
		fmt.Printf("  Lowest note            : %s  (%.1f Hz)\n", lowest.Note, lowest.Frequency)
		fmt.Printf("  Highest note           : %s  (%.1f Hz)\n", highest.Note, highest.Frequency)
		fmt.Printf("  Comfortable range      : %s → %s  (P10–P90)\n", voice.LowNote, voice.HighNote)
		fmt.Printf("  Median pitch           : %s  (%.1f Hz)\n", voice.MedianNote, voice.MedianFreq)
		fmt.Printf("  Estimated voice type   : %s\n", voice.Name)
		fmt.Printf("  Pitch detections       : %d\n", len(allPitches))
		fmt.Println()
	} else {
		fmt.Println("  (no pitch detected — try singing a sustained note)")
		fmt.Println()
	}

	// Per-segment breakdown.
	fmt.Println("  ── Segments ─────────────────────────────────────────")
	for i, seg := range segments {
		pitchInfo := "no pitch"
		if len(seg.pitches) > 0 {
			lo, hi := pitchRange(seg.pitches)
			if lo.Note == hi.Note {
				pitchInfo = lo.Note
			} else {
				pitchInfo = fmt.Sprintf("%s → %s", lo.Note, hi.Note)
			}
		}
		fmt.Printf("    %2d. %s → %s  (%s)  [%s]\n",
			i+1,
			seg.start.Format("15:04:05.000"),
			seg.end.Format("15:04:05.000"),
			seg.duration.Round(time.Millisecond),
			pitchInfo,
		)
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
}

// pitchRange returns the lowest and highest pitch results by MIDI note number.
func pitchRange(pitches []pitch.Result) (lowest, highest pitch.Result) {
	lowest = pitches[0]
	highest = pitches[0]
	for _, p := range pitches[1:] {
		if p.MIDINote < lowest.MIDINote {
			lowest = p
		}
		if p.MIDINote > highest.MIDINote {
			highest = p
		}
	}
	return lowest, highest
}
