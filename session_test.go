package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/etimbukafia/real-time-voice-pipeline-go/llm"

	"github.com/etimbukafia/speech-coach/pitch"
)

func TestInferPracticeContext(t *testing.T) {
	turns := []TurnRecord{
		{UserText: "I need to sound steadier for a job interview tomorrow."},
	}

	got := inferPracticeContext(turns)
	if got != "job interview practice" {
		t.Fatalf("inferPracticeContext() = %q, want %q", got, "job interview practice")
	}
}

func TestBuildCoachingSignals(t *testing.T) {
	baseline := &pitch.VoiceType{MedianFreq: 145}
	turns := []TurnRecord{
		{MedianFreq: 150, UserText: "Just chatting."},
		{MedianFreq: 158, UserText: "Still talking."},
		{MedianFreq: 160, UserText: "I'm on a phone call with a client."},
	}
	messages := []llm.Message{
		{Role: "assistant", Content: "That was better. Nice and steady."},
		{Role: "assistant", Content: "Try slowing down and relax your throat."},
	}

	signals := buildCoachingSignals(baseline, turns, messages)
	if signals.ConsecutiveHighTurns != 2 {
		t.Fatalf("ConsecutiveHighTurns = %d, want 2", signals.ConsecutiveHighTurns)
	}
	if signals.TurnsSinceCorrection != 0 {
		t.Fatalf("TurnsSinceCorrection = %d, want 0", signals.TurnsSinceCorrection)
	}
	if signals.TurnsSincePraise != 1 {
		t.Fatalf("TurnsSincePraise = %d, want 1", signals.TurnsSincePraise)
	}
	if signals.PracticeContext != "work conversation practice" {
		t.Fatalf("PracticeContext = %q, want %q", signals.PracticeContext, "work conversation practice")
	}
	if signals.CarryoverSuggestion == "" {
		t.Fatal("CarryoverSuggestion should not be empty")
	}
}

func TestBuildSessionMemory(t *testing.T) {
	baseline := &pitch.VoiceType{MedianFreq: 145}
	turns := []TurnRecord{
		{MedianFreq: 158, UserText: "I have a presentation this week."},
		{MedianFreq: 160, UserText: "I want to sound more grounded when I present."},
	}
	messages := []llm.Message{
		{Role: "assistant", Content: "Try slowing down and relax your throat. Keep the ending flatter."},
		{Role: "assistant", Content: "Good. That sounded steadier."},
	}
	signals := buildCoachingSignals(baseline, turns, messages)
	memory := buildSessionMemory(SessionMemory{}, baseline, turns, messages, signals)

	for _, snippet := range []string{
		"carry a deeper, steadier voice in presentations",
		"presentation practice",
		"Try slowing down and relax your throat.",
	} {
		if !strings.Contains(memory.UserGoal+" "+memory.PracticeContext+" "+memory.LastCue, snippet) {
			t.Fatalf("buildSessionMemory() missing %q in %#v", snippet, memory)
		}
	}
	if len(memory.RecurringIssues) == 0 {
		t.Fatalf("buildSessionMemory() should infer at least one recurring issue: %#v", memory)
	}
}

func TestBuildSessionContextIncludesStructuredMemory(t *testing.T) {
	turns := []TurnRecord{{MedianFreq: 160, UserText: "I have a presentation this week."}}
	messages := []llm.Message{{Role: "assistant", Content: "Try slowing down and relax your throat."}}
	signals := buildCoachingSignals(&pitch.VoiceType{MedianFreq: 145}, turns, messages)
	memory := buildSessionMemory(SessionMemory{}, &pitch.VoiceType{MedianFreq: 145}, turns, messages, signals)
	context := buildSessionContext(
		time.Now().Add(-10*time.Minute),
		&pitch.VoiceType{MedianFreq: 145, MedianNote: "D3", Name: "Baritone", LowNote: "B2", HighNote: "F3"},
		SessionStats{},
		nil,
		memory,
		signals,
	)

	for _, snippet := range []string{
		"Conversation policy: stay natural and user-led; do not correct every turn.",
		"Session memory: goal=",
		`context="presentation practice"`,
		"Coaching cadence: pitch_trend=",
		"The last coach turn already gave a corrective cue.",
		"Do not assign homework every turn.",
	} {
		if !strings.Contains(context, snippet) {
			t.Fatalf("buildSessionContext() missing %q in %q", snippet, context)
		}
	}
}

func TestBuildSessionContextAddsCarryoverAndSelfMonitoringGuidance(t *testing.T) {
	turns := []TurnRecord{
		{MedianFreq: 150, UserText: "I have an interview tomorrow."},
		{MedianFreq: 154, UserText: "I'm practicing answers."},
		{MedianFreq: 156, UserText: "I want to sound steady."},
	}
	messages := []llm.Message{
		{Role: "assistant", Content: "Good. That sounded steadier."},
		{Role: "assistant", Content: "Keep going."},
		{Role: "assistant", Content: "Tell me more."},
	}
	signals := buildCoachingSignals(&pitch.VoiceType{MedianFreq: 145}, turns, messages)
	memory := buildSessionMemory(SessionMemory{}, &pitch.VoiceType{MedianFreq: 145}, turns, messages, signals)
	context := buildSessionContext(
		time.Now().Add(-15*time.Minute),
		&pitch.VoiceType{MedianFreq: 145, MedianNote: "D3", Name: "Baritone", LowNote: "B2", HighNote: "F3"},
		SessionStats{Turns: 4},
		nil,
		memory,
		signals,
	)

	for _, snippet := range []string{
		"consider one brief self-monitoring question",
		"Suggested carryover: answer one practice interview question out loud later today and keep the same slower, steadier delivery.",
	} {
		if !strings.Contains(context, snippet) {
			t.Fatalf("buildSessionContext() missing %q in %q", snippet, context)
		}
	}
}

func TestStoreSessionMemoryRoundTrip(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "coach.db"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	defer store.Close()

	const sessionID = "session-memory-roundtrip"
	if err := store.CreateSession(sessionID); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	memory := SessionMemory{
		UserGoal:        "sound steadier on calls",
		PracticeContext: "phone call practice",
		RecurringIssues: []string{"pitch drifts high across consecutive turns"},
		RecentWins:      []string{"recent turns are landing lower"},
		LastCue:         "Slow down and let the end of the sentence settle.",
		WorkingCue:      "Slow down and let the end of the sentence settle.",
		CarryoverFocus:  "use this same steady voice on your next phone call and keep the first two sentences relaxed",
		UpdatedAt:       time.Now().UTC(),
	}
	if err := store.SaveSessionMemory(sessionID, memory); err != nil {
		t.Fatalf("SaveSessionMemory() error = %v", err)
	}

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatalf("LoadSession() error = %v", err)
	}
	decoded, err := decodeSessionMemory(loaded.MemoryJSON)
	if err != nil {
		t.Fatalf("decodeSessionMemory() error = %v", err)
	}
	if !sessionMemoryEqual(memory, decoded) {
		t.Fatalf("session memory mismatch: got %#v want %#v", decoded, memory)
	}
}
