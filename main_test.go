package main

import (
	"io"
	"log"
	"testing"

	"github.com/etimbukafia/real-time-voice-pipeline-go/stt"
)

func TestNewSTTAssemblyAI(t *testing.T) {
	t.Setenv("VOICE_COACH_STT_PROVIDER", "assemblyai")
	t.Setenv("ASSEMBLYAI_API_KEY", "test-assembly-key")
	t.Setenv("ASSEMBLYAI_STREAMING_URL", "wss://streaming.assemblyai.com/v3/ws")
	t.Setenv("ASSEMBLYAI_SPEECH_MODEL", "universal-streaming-english")
	t.Setenv("ASSEMBLYAI_FORMAT_TURNS", "true")
	t.Setenv("ASSEMBLYAI_KEYTERMS_PROMPT", "Rowan, resonance, grounded")

	client, err := newSTT(log.New(io.Discard, "", 0), "unused-mistral-key")
	if err != nil {
		t.Fatalf("newSTT() error = %v", err)
	}
	if _, ok := client.(*stt.AssemblyAIStreamingSTT); !ok {
		t.Fatalf("newSTT() type = %T, want *stt.AssemblyAIStreamingSTT", client)
	}
}

func TestNewSTTAssemblyAIRequiresAPIKey(t *testing.T) {
	t.Setenv("VOICE_COACH_STT_PROVIDER", "assemblyai")
	t.Setenv("ASSEMBLYAI_API_KEY", "")

	_, err := newSTT(log.New(io.Discard, "", 0), "unused-mistral-key")
	if err == nil {
		t.Fatal("newSTT() error = nil, want AssemblyAI API key validation error")
	}
	if got := err.Error(); got != "ASSEMBLYAI_API_KEY is required when VOICE_COACH_STT_PROVIDER=assemblyai" {
		t.Fatalf("newSTT() error = %q", got)
	}
}

func TestEnvCSV(t *testing.T) {
	t.Setenv("ASSEMBLYAI_KEYTERMS_PROMPT", "Rowan, resonance;\ngrounded")

	got := envCSV("ASSEMBLYAI_KEYTERMS_PROMPT")
	want := []string{"Rowan", "resonance", "grounded"}
	if len(got) != len(want) {
		t.Fatalf("envCSV() len = %d, want %d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("envCSV()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
