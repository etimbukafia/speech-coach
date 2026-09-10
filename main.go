// speech-coach/main.go
//
// Two modes:
//   --mode=analyze  (default) Standalone pitch analysis — records and reports vocal range.
//   --mode=coach    Deep voice speech coach — real-time conversation with pitch feedback.
//   --mode=browser  Browser-based deep voice coach — microphone and playback in the browser.
//
// Requires:
//   VOICE_COACH_VAD_PROVIDER = energy or silero (default: energy on Windows, silero elsewhere)
//   SILERO_MODEL_PATH        = path to silero_vad.onnx when VOICE_COACH_VAD_PROVIDER=silero
//   VOICE_COACH_PYTHON_EXE   = Python executable used for the Silero worker (default: python)
//
// Coach mode additionally requires:
//   MISTRAL_API_KEY    = Mistral API key (for STT + LLM)
//   MISTRAL_BASE_URL   = Mistral API base URL (default: https://api.mistral.ai)
//   CARTESIA_API_KEY   = Cartesia API key (for TTS)
//   CARTESIA_VOICE_ID  = Cartesia voice ID
//
// Optional coach overrides:
//   VOICE_COACH_STT_PROVIDER     = python-mistral, go-websocket, or assemblyai (default: python-mistral)
//   VOICE_COACH_STT_PYTHON_EXE   = Python executable used for the Mistral STT worker
//   VOXTRAL_TARGET_STREAMING_DELAY_MS = Mistral realtime target delay in milliseconds
//   VOXTRAL_REALTIME_URL         = Full realtime WebSocket URL for the legacy Go websocket client
//   ASSEMBLYAI_API_KEY           = AssemblyAI API key when VOICE_COACH_STT_PROVIDER=assemblyai
//   ASSEMBLYAI_STREAMING_URL     = AssemblyAI streaming WebSocket URL (default: wss://streaming.assemblyai.com/v3/ws)
//   ASSEMBLYAI_SPEECH_MODEL      = AssemblyAI speech model (default: universal-streaming-english)
//   ASSEMBLYAI_FORMAT_TURNS      = Whether to request formatted final turns (default: true)
//   ASSEMBLYAI_VAD_THRESHOLD     = AssemblyAI VAD silence threshold (default: 0.4)
//   ASSEMBLYAI_END_OF_TURN_CONFIDENCE_THRESHOLD = AssemblyAI end-of-turn confidence threshold (default: 0.7)
//   ASSEMBLYAI_MIN_TURN_SILENCE_MS = AssemblyAI minimum silence before semantic turn end (default: 800)
//   ASSEMBLYAI_MAX_TURN_SILENCE_MS = AssemblyAI maximum silence before acoustic turn end (default: 3600)
//   ASSEMBLYAI_KEYTERMS_PROMPT   = Optional comma-separated boosted terms (for example: Rowan, resonance, grounded)
//
// Build:
//   go build -tags silero,portaudio -o speech-coach.exe .

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/etimbukafia/real-time-voice-pipeline-go/audio"
	"github.com/etimbukafia/real-time-voice-pipeline-go/llm"
	"github.com/etimbukafia/real-time-voice-pipeline-go/playback"
	"github.com/etimbukafia/real-time-voice-pipeline-go/stt"
	"github.com/etimbukafia/real-time-voice-pipeline-go/tts"
	"github.com/etimbukafia/real-time-voice-pipeline-go/vad"

	"github.com/etimbukafia/speech-coach/pitch"
)

func main() {
	mode := flag.String("mode", "analyze", "Mode: analyze, coach, or browser")
	dbPath := flag.String("db", "", "SQLite database path for coach sessions (default: ~/.voice-coach/coach.db)")
	listenAddr := flag.String("listen", "127.0.0.1:8080", "HTTP listen address for browser mode")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	if err := loadDotEnv(); err != nil {
		logger.Fatalf("failed to load .env files: %v", err)
	}

	// Signal handling: Ctrl-C triggers graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println("\n⏹  Stopping…")
		cancel()
	}()

	switch strings.ToLower(*mode) {
	case "browser":
		store, err := OpenStore(*dbPath)
		if err != nil {
			logger.Fatalf("failed to open session store: %v", err)
		}
		defer store.Close()

		if err := runBrowserServer(ctx, logger, *listenAddr, store); err != nil && err != context.Canceled {
			logger.Fatalf("browser session failed: %v", err)
		}
		return
	}

	// Microphone
	mic, err := audio.NewMicSource()
	if err != nil {
		logger.Fatalf("failed to open microphone: %v", err)
	}

	// VAD
	detector, err := newVAD(logger)
	if err != nil {
		logger.Fatalf("failed to create VAD: %v", err)
	}
	defer detector.Destroy()

	tracker := vad.NewStateTracker(detector, 80, 400)

	// Pitch detector (YIN algorithm)
	pitchDetector := pitch.NewDetector(audio.SampleRate, 75.0, 800.0, 0.15)

	switch strings.ToLower(*mode) {
	case "analyze":
		runAnalyze(ctx, logger, mic, tracker, pitchDetector)
	case "coach":
		store, err := OpenStore(*dbPath)
		if err != nil {
			logger.Fatalf("failed to open session store: %v", err)
		}
		defer store.Close()

		session, err := NewSession(store)
		if err != nil {
			logger.Fatalf("failed to create session: %v", err)
		}

		runCoach(ctx, logger, mic, tracker, pitchDetector, session)
	default:
		logger.Fatalf("unknown mode %q; expected 'analyze', 'coach', or 'browser'", *mode)
	}
}

func loadDotEnv() error {
	paths := []string{".env.local", ".env"}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := godotenv.Load(path); err != nil {
			return err
		}
	}
	return nil
}

// runAnalyze is the standalone pitch analysis mode (original behavior).
func runAnalyze(ctx context.Context, logger *log.Logger, mic audio.AudioSource, tracker *vad.StateTracker, pitchDetector *pitch.Detector) {
	segments, frameCount, err := RunVADSession(ctx, mic, tracker, pitchDetector)
	if err != nil {
		logger.Fatalf("session failed: %v", err)
	}
	PrintSummary(segments, frameCount)
}

// runCoach builds the full pipeline components and starts the coaching session.
func runCoach(ctx context.Context, logger *log.Logger, mic audio.AudioSource, tracker *vad.StateTracker, pitchDetector *pitch.Detector, session *Session) {
	coach, cleanup, err := newCoachFoundation(ctx, logger, session, true)
	if err != nil {
		logger.Fatalf("failed to create coach foundation: %v", err)
	}
	defer cleanup()

	coach.Source = mic
	coach.Tracker = tracker
	coach.Pitch = pitchDetector

	if err := coach.Run(ctx); err != nil && err != context.Canceled {
		logger.Fatalf("coach session failed: %v", err)
	}
}

func newCoachFoundation(ctx context.Context, logger *log.Logger, session *Session, withPlayer bool) (*Coach, func(), error) {
	mistralKey := requireEnv(logger, "MISTRAL_API_KEY")
	sttClient, err := newSTT(logger, mistralKey)
	if err != nil {
		return nil, nil, err
	}

	llmModel := envOr("MISTRAL_LLM_MODEL", "mistral-small-latest")
	llmClient, err := llm.NewMistralClient(mistralKey, llmModel)
	if err != nil {
		closeProviderIfSupported(sttClient)
		return nil, nil, err
	}

	cartesiaKey := requireEnv(logger, "CARTESIA_API_KEY")
	cartesiaVoice := requireEnv(logger, "CARTESIA_VOICE_ID")
	ttsEngine, err := tts.NewCartesiaEngine(cartesiaKey)
	if err != nil {
		closeProviderIfSupported(sttClient)
		return nil, nil, err
	}
	ttsEngine.Version = envOr("CARTESIA_VERSION", "2025-04-16")
	ttsEngine.ModelID = envOr("CARTESIA_MODEL_ID", "sonic-3")
	ttsEngine.VoiceID = cartesiaVoice
	ttsEngine.Language = envOr("CARTESIA_LANGUAGE", "en")

	var player playback.Player
	if withPlayer {
		player, err = playback.NewPortAudioPlayer()
		if err != nil {
			closeProviderIfSupported(sttClient)
			closeProviderIfSupported(ttsEngine)
			return nil, nil, err
		}
	}

	warmCtx, warmCancel := context.WithTimeout(ctx, 10*time.Second)
	defer warmCancel()
	if err := warmProviderIfSupported(warmCtx, sttClient); err != nil {
		closeProviderIfSupported(sttClient)
		closeProviderIfSupported(ttsEngine)
		return nil, nil, err
	}
	if err := warmProviderIfSupported(warmCtx, ttsEngine); err != nil {
		closeProviderIfSupported(sttClient)
		closeProviderIfSupported(ttsEngine)
		return nil, nil, err
	}

	coach := &Coach{
		STT:         sttClient,
		LLM:         llmClient,
		TTS:         ttsEngine,
		Player:      player,
		Logger:      logger,
		LLMModel:    llmModel,
		TTSModel:    ttsEngine.ModelID,
		VoiceID:     cartesiaVoice,
		Language:    ttsEngine.Language,
		Temperature: 0.3,
		MaxTokens:   80,
		ConsoleFeed: envBool("VOICE_COACH_CONSOLE", false),
		Session:     session,
		Tools:       NewCoachTools(session),
	}
	cleanup := func() {
		closeProviderIfSupported(sttClient)
		closeProviderIfSupported(ttsEngine)
	}
	return coach, cleanup, nil
}

// requireEnv reads a required environment variable or exits with an error.
func requireEnv(logger *log.Logger, key string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		logger.Fatalf("%s is required for coach mode", key)
	}
	return val
}

// envOr reads an environment variable with a fallback default.
func envOr(key, fallback string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	return val
}

func envBool(key string, fallback bool) bool {
	val := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch val {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return fallback
	default:
		return fallback
	}
}

func envFloat(key string, fallback float64) float64 {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(val)
	if err != nil {
		return fallback
	}
	return parsed
}

func envCSV(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == ';'
	})
	values := make([]string, 0, len(fields))
	for _, field := range fields {
		if cleaned := strings.TrimSpace(field); cleaned != "" {
			values = append(values, cleaned)
		}
	}
	return values
}

func newVAD(logger *log.Logger) (vad.VAD, error) {
	provider := strings.ToLower(envOr("VOICE_COACH_VAD_PROVIDER", defaultVADProvider()))
	switch provider {
	case "energy":
		threshold := envFloat("VOICE_COACH_ENERGY_THRESHOLD", 0.04)
		logger.Printf("using energy VAD (threshold %.3f)", threshold)
		return vad.NewEnergyVAD(threshold)
	case "silero":
		modelPath := strings.TrimSpace(os.Getenv("SILERO_MODEL_PATH"))
		if modelPath == "" {
			return nil, fmt.Errorf("SILERO_MODEL_PATH is required when VOICE_COACH_VAD_PROVIDER=silero")
		}
		pythonExe := envOr("VOICE_COACH_PYTHON_EXE", "python")
		threshold := envFloat("VOICE_COACH_SILERO_THRESHOLD", 0.5)
		logger.Printf("using Silero VAD via Python worker (%s)", pythonExe)
		return vad.NewPythonSileroVAD(vad.PythonSileroConfig{
			PythonExe:  pythonExe,
			ModelPath:  modelPath,
			SampleRate: audio.SampleRate,
			Threshold:  threshold,
		})
	default:
		return nil, fmt.Errorf("unknown VOICE_COACH_VAD_PROVIDER %q; expected `energy` or `silero`", provider)
	}
}

func defaultVADProvider() string {
	if runtime.GOOS == "windows" {
		return "energy"
	}
	return "silero"
}

func newSTT(logger *log.Logger, apiKey string) (stt.STT, error) {
	provider := strings.ToLower(envOr("VOICE_COACH_STT_PROVIDER", "python-mistral"))
	switch provider {
	case "python-mistral":
		pythonExe := envOr("VOICE_COACH_STT_PYTHON_EXE", envOr("VOICE_COACH_PYTHON_EXE", "python"))
		logger.Printf("using Mistral realtime STT via Python worker (%s)", pythonExe)
		return stt.NewPythonMistralRealtimeSTT(stt.PythonMistralRealtimeConfig{
			PythonExe:              pythonExe,
			APIKey:                 apiKey,
			Model:                  envOr("VOXTRAL_MODEL", "voxtral-mini-transcribe-realtime-2602"),
			ServerURL:              resolveMistralBaseURL(),
			TargetStreamingDelayMS: envInt("VOXTRAL_TARGET_STREAMING_DELAY_MS", 0),
		})
	case "go-websocket":
		header := http.Header{}
		header.Set("Authorization", "Bearer "+apiKey)
		return stt.NewMistralVoxtralSTT(
			envOr("VOXTRAL_MODEL", "voxtral-mini-transcribe-realtime-2602"),
			resolveVoxtralRealtimeURL(),
			header,
		)
	case "assemblyai":
		assemblyKey := strings.TrimSpace(os.Getenv("ASSEMBLYAI_API_KEY"))
		if assemblyKey == "" {
			return nil, fmt.Errorf("ASSEMBLYAI_API_KEY is required when VOICE_COACH_STT_PROVIDER=assemblyai")
		}
		logger.Printf("using AssemblyAI streaming STT (%s)", envOr("ASSEMBLYAI_SPEECH_MODEL", "universal-streaming-english"))
		return stt.NewAssemblyAIStreamingSTT(stt.AssemblyAIStreamingConfig{
			APIKey:                       assemblyKey,
			RealtimeURL:                  envOr("ASSEMBLYAI_STREAMING_URL", "wss://streaming.assemblyai.com/v3/ws"),
			SpeechModel:                  envOr("ASSEMBLYAI_SPEECH_MODEL", "universal-streaming-english"),
			SampleRate:                   audio.SampleRate,
			FormatTurns:                  envBool("ASSEMBLYAI_FORMAT_TURNS", true),
			VADThreshold:                 envFloat("ASSEMBLYAI_VAD_THRESHOLD", 0.4),
			EndOfTurnConfidenceThreshold: envFloat("ASSEMBLYAI_END_OF_TURN_CONFIDENCE_THRESHOLD", 0.7),
			MinTurnSilenceMS:             envInt("ASSEMBLYAI_MIN_TURN_SILENCE_MS", 800),
			MaxTurnSilenceMS:             envInt("ASSEMBLYAI_MAX_TURN_SILENCE_MS", 3600),
			KeytermsPrompt:               envCSV("ASSEMBLYAI_KEYTERMS_PROMPT"),
		})
	default:
		return nil, fmt.Errorf("unknown VOICE_COACH_STT_PROVIDER %q; expected `python-mistral`, `go-websocket`, or `assemblyai`", provider)
	}
}

func resolveMistralBaseURL() string {
	if explicit := strings.TrimSpace(os.Getenv("MISTRAL_BASE_URL")); explicit != "" {
		return normalizeMistralBaseURL(explicit)
	}
	if legacy := strings.TrimSpace(os.Getenv("VOXTRAL_REALTIME_URL")); legacy != "" {
		return normalizeMistralBaseURL(legacy)
	}
	return "https://api.mistral.ai"
}

func resolveVoxtralRealtimeURL() string {
	if explicit := strings.TrimSpace(os.Getenv("VOXTRAL_REALTIME_URL")); explicit != "" {
		return explicit
	}

	base := envOr("MISTRAL_BASE_URL", "https://api.mistral.ai")
	parsed, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/")
	}

	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	}

	return strings.TrimRight(parsed.String(), "/")
}

func normalizeMistralBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "https://api.mistral.ai"
	}
	trimmed = strings.TrimRight(trimmed, "/")
	switch {
	case strings.HasPrefix(trimmed, "wss://"):
		return "https://" + strings.TrimPrefix(trimmed, "wss://")
	case strings.HasPrefix(trimmed, "ws://"):
		return "http://" + strings.TrimPrefix(trimmed, "ws://")
	case strings.HasPrefix(trimmed, "https://"), strings.HasPrefix(trimmed, "http://"):
		return trimmed
	default:
		return "https://" + trimmed
	}
}

func warmProviderIfSupported(ctx context.Context, provider any) error {
	type warmable interface {
		Warm(context.Context) error
	}
	warm, ok := provider.(warmable)
	if !ok {
		return nil
	}
	return warm.Warm(ctx)
}

func closeProviderIfSupported(provider any) {
	type closeable interface {
		Close() error
	}
	closer, ok := provider.(closeable)
	if !ok {
		return
	}
	_ = closer.Close()
}
