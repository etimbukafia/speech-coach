package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/etimbukafia/real-time-voice-pipeline-go/pipeline"

	"github.com/etimbukafia/speech-coach/pitch"
)

func TestWaitForTranscriptUsesFinalCommit(t *testing.T) {
	coach := &Coach{}
	commits := make(chan pipeline.TranscriptCommit, 2)
	out := make(chan transcriptDone, 1)
	pitches := &pitchAccum{}
	pitches.Add(pitch.Result{Frequency: 145})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go coach.waitForTranscript(ctx, 3, commits, pitches, out)
	commits <- pipeline.TranscriptCommit{Text: "what's your", Final: false, Timestamp: time.Now()}
	commits <- pipeline.TranscriptCommit{Text: "what's your name", Final: true, Timestamp: time.Now()}
	close(commits)

	result := <-out
	if result.err != nil {
		t.Fatalf("waitForTranscript() err = %v", result.err)
	}
	if result.text != "what's your name" {
		t.Fatalf("waitForTranscript() text = %q, want %q", result.text, "what's your name")
	}
	if len(result.pitches) != 1 {
		t.Fatalf("waitForTranscript() pitches = %d, want 1", len(result.pitches))
	}
}

func TestWaitForTranscriptFallsBackToStablePartial(t *testing.T) {
	coach := &Coach{}
	commits := make(chan pipeline.TranscriptCommit, 2)
	out := make(chan transcriptDone, 1)
	pitches := &pitchAccum{}
	pitches.Add(pitch.Result{Frequency: 145})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go coach.waitForTranscript(ctx, 4, commits, pitches, out)
	commits <- pipeline.TranscriptCommit{Text: "what's your name", Final: false, Timestamp: time.Now()}
	close(commits)

	result := <-out
	if result.err != nil {
		t.Fatalf("waitForTranscript() err = %v", result.err)
	}
	if result.text != "what's your name" {
		t.Fatalf("waitForTranscript() text = %q, want %q", result.text, "what's your name")
	}
	if len(result.pitches) != 1 {
		t.Fatalf("waitForTranscript() pitches = %d, want 1", len(result.pitches))
	}
}

func TestWaitForTranscriptPropagatesCommitError(t *testing.T) {
	coach := &Coach{}
	commits := make(chan pipeline.TranscriptCommit, 1)
	out := make(chan transcriptDone, 1)
	pitches := &pitchAccum{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go coach.waitForTranscript(ctx, 5, commits, pitches, out)
	commits <- pipeline.TranscriptCommit{Err: errors.New("stt: rate limited"), Timestamp: time.Now()}
	close(commits)

	result := <-out
	if result.err == nil {
		t.Fatal("waitForTranscript() err = nil, want propagated error")
	}
	if result.err.Error() != "stt: rate limited" {
		t.Fatalf("waitForTranscript() err = %q, want %q", result.err.Error(), "stt: rate limited")
	}
}
