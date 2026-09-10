package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/etimbukafia/real-time-voice-pipeline-go/audio"
	"github.com/etimbukafia/real-time-voice-pipeline-go/llm"
	"github.com/etimbukafia/real-time-voice-pipeline-go/pipeline"
	"github.com/etimbukafia/real-time-voice-pipeline-go/playback"
	"github.com/etimbukafia/real-time-voice-pipeline-go/stt"
	"github.com/etimbukafia/real-time-voice-pipeline-go/tts"
	"github.com/etimbukafia/real-time-voice-pipeline-go/vad"

	"github.com/etimbukafia/speech-coach/pitch"
)

type Coach struct {
	Source  audio.AudioSource
	Tracker *vad.StateTracker
	Pitch   *pitch.Detector
	STT     stt.STT
	LLM     llm.Client
	TTS     tts.Engine
	Player  playback.Player
	Logger  *log.Logger

	Session *Session
	Tools   *CoachTools

	LLMModel    string
	TTSModel    string
	VoiceID     string
	Language    string
	Temperature float64
	MaxTokens   int
	ConsoleFeed bool
}

func (c *Coach) Run(ctx context.Context) error {
	logger := c.logger()
	c.consolef("Deep Voice Coach ready. Press Ctrl-C to end the session.\n")

	if c.Session != nil {
		defer func() {
			if err := c.Session.PrintReport(); err != nil {
				logger.Printf("coach: failed to print session report: %v", err)
			}
		}()
	}

	if err := c.speakText(ctx, calibrationPrompt, false); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("coach: calibration prompt TTS failed: %v", err)
	}
	c.consolef("Calibration prompt spoken. Say it once in your natural voice.\n")

	frames, err := c.Source.Stream(ctx)
	if err != nil {
		return fmt.Errorf("coach: failed to start audio stream: %w", err)
	}

	var (
		turnSeq int

		currentCtx    context.Context
		currentCancel context.CancelFunc
		audioIn       chan *audio.AudioFrame
		captureClosed bool
		isSpeaking    bool
		pitchBuf      []byte
		pitches       *pitchAccum
		calibrating   = true

		transcriptCh = make(chan transcriptDone, 1)
		responseCh   = make(chan responseDone, 1)
	)

	defer func() {
		if currentCancel != nil {
			currentCancel()
		}
	}()

	for frames != nil {
		select {
		case <-ctx.Done():
			if currentCancel != nil {
				currentCancel()
			}
			return ctx.Err()

		case result := <-transcriptCh:
			if result.err != nil {
				if !errors.Is(result.err, context.Canceled) {
					logger.Printf("coach: STT error: %v", result.err)
				}
				continue
			}

			if strings.TrimSpace(result.text) == "" {
				continue
			}

			if calibrating {
				if err := c.finishCalibration(ctx, result.pitches); err != nil {
					logger.Printf("coach: calibration failed: %v", err)
					_ = c.speakText(ctx, "I didn't get a strong pitch sample. Please repeat the baseline sentence.", false)
					c.consolef("Calibration retry requested.\n")
					if currentCancel != nil {
						currentCancel()
						currentCancel = nil
					}
					continue
				}
				calibrating = false
				if currentCancel != nil {
					currentCancel()
					currentCancel = nil
				}
				continue
			}

			turnRecord := BuildTurnRecord(result.turnIndex, result.text, result.pitches)
			if c.Session != nil {
				if err := c.Session.SaveTurn(turnRecord); err != nil {
					logger.Printf("coach: failed to save turn %d: %v", turnRecord.TurnIndex, err)
				}
			}

			logger.Printf("coach: user said %q", result.text)
			logger.Printf("coach: %s", turnRecord.PitchReport)

			userMessage := llm.Message{
				Role:    "user",
				Content: result.text + "\n\n[PITCH ANALYSIS] " + turnRecord.PitchReport,
			}

			messages := []llm.Message{{Role: "system", Content: "Session context unavailable."}}
			if c.Session != nil {
				msgs, err := c.Session.BuildMessages(8)
				if err != nil {
					logger.Printf("coach: failed to build session context: %v", err)
				} else {
					messages = msgs
				}
			}
			messages = append(messages, userMessage)

			go func(turnCtx context.Context, rec TurnRecord, user llm.Message, convo []llm.Message) {
				resp := c.runResponse(turnCtx, rec, user, convo)
				select {
				case <-turnCtx.Done():
				case responseCh <- resp:
				}
			}(currentCtx, turnRecord, userMessage, messages)

		case result := <-responseCh:
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				logger.Printf("coach: response error: %v", result.err)
			}

			if result.text != "" && c.Session != nil {
				result.turnRecord.CoachText = result.text
				if err := c.Session.SaveTurn(result.turnRecord); err != nil {
					logger.Printf("coach: failed to update turn %d with coach reply: %v", result.turnRecord.TurnIndex, err)
				}
				c.Session.AppendConversation(result.userMessage, llm.Message{
					Role:    "assistant",
					Content: result.text,
				})
			}

			if currentCancel != nil {
				currentCancel()
				currentCancel = nil
			}

		case frame, ok := <-frames:
			if !ok {
				frames = nil
				if audioIn != nil && !captureClosed {
					close(audioIn)
					captureClosed = true
				}
				continue
			}
			if frame == nil {
				continue
			}

			event, _, err := c.Tracker.Process(frame)
			if err != nil {
				logger.Printf("coach: VAD error: %v", err)
				continue
			}

			switch event {
			case vad.SpeechStart:
				if currentCancel != nil {
					currentCancel()
				}

				turnSeq++
				currentCtx, currentCancel = context.WithCancel(ctx)
				audioIn = make(chan *audio.AudioFrame, 32)
				captureClosed = false
				isSpeaking = true
				pitchBuf = pitchBuf[:0]
				pitches = &pitchAccum{}

				if calibrating {
					c.consolef("Listening for baseline...\n")
				} else {
					c.consolef("Listening... (turn %d)\n", turnSeq)
				}
				logger.Printf("coach: turn %d speech start", turnSeq)

				transcripts, sttErr := c.STT.Transcribe(currentCtx, audioIn)
				if sttErr != nil {
					logger.Printf("coach: STT start error: %v", sttErr)
					isSpeaking = false
					if currentCancel != nil {
						currentCancel()
						currentCancel = nil
					}
					continue
				}

				stabilizer := pipeline.NewTranscriptStabilizer(2)
				commits := stabilizer.Run(currentCtx, transcripts)
				go c.waitForTranscript(currentCtx, turnSeq, commits, pitches, transcriptCh)

				sendFrame(audioIn, frame)

			case vad.SpeechEnd:
				if isSpeaking && audioIn != nil && !captureClosed {
					logger.Printf("coach: turn %d speech end", turnSeq)
					if calibrating {
						c.consolef("Processing baseline...\n")
					} else {
						c.consolef("Processing...\n")
					}
					close(audioIn)
					captureClosed = true
					isSpeaking = false
				}

			default:
				if isSpeaking && audioIn != nil && !captureClosed {
					sendFrame(audioIn, frame)
				}
			}

			if isSpeaking && frame.Data != nil {
				pitchBuf = append(pitchBuf, frame.Data...)
				if len(pitchBuf) > 2*audio.FrameSize {
					pitchBuf = pitchBuf[len(pitchBuf)-2*audio.FrameSize:]
				}
				if len(pitchBuf) >= 2*audio.FrameSize && pitches != nil {
					result := c.Pitch.Detect(pitchBuf)
					if result.MIDINote >= 0 && result.Confidence > 0.5 {
						pitches.Add(result)
					}
				}
			}
		}
	}

	if currentCancel != nil {
		currentCancel()
	}
	return nil
}

type transcriptDone struct {
	turnIndex  int
	text       string
	pitches    []pitch.Result
	confidence float64
	err        error
}

type responseDone struct {
	turnRecord  TurnRecord
	userMessage llm.Message
	text        string
	err         error
}

type pitchAccum struct {
	mu      sync.Mutex
	results []pitch.Result
}

func (a *pitchAccum) Add(r pitch.Result) {
	a.mu.Lock()
	a.results = append(a.results, r)
	a.mu.Unlock()
}

func (a *pitchAccum) Snapshot() []pitch.Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]pitch.Result, len(a.results))
	copy(out, a.results)
	return out
}

func (a *pitchAccum) Reset() {
	a.mu.Lock()
	a.results = a.results[:0]
	a.mu.Unlock()
}

func (c *Coach) waitForTranscript(
	ctx context.Context,
	turnIndex int,
	commits <-chan pipeline.TranscriptCommit,
	pitches *pitchAccum,
	out chan<- transcriptDone,
) {
	var (
		sawFinal       bool
		lastCommit     string
		lastSnapshot   []pitch.Result
		lastConfidence float64
	)
	for commit := range commits {
		if commit.Err != nil {
			select {
			case <-ctx.Done():
			case out <- transcriptDone{turnIndex: turnIndex, err: commit.Err}:
			}
			return
		}
		if strings.TrimSpace(commit.Text) != "" {
			lastCommit = commit.Text
			lastSnapshot = pitches.Snapshot()
			lastConfidence = commit.Confidence
		}
		if !commit.Final {
			continue
		}
		sawFinal = true

		select {
		case <-ctx.Done():
		case out <- transcriptDone{
			turnIndex:  turnIndex,
			text:       commit.Text,
			pitches:    pitches.Snapshot(),
			confidence: commit.Confidence,
		}:
		}
		return
	}

	if ctx.Err() != nil {
		select {
		case <-ctx.Done():
		case out <- transcriptDone{turnIndex: turnIndex, err: ctx.Err()}:
		}
		return
	}
	if !sawFinal {
		if strings.TrimSpace(lastCommit) != "" {
			select {
			case <-ctx.Done():
			case out <- transcriptDone{
				turnIndex:  turnIndex,
				text:       lastCommit,
				pitches:    lastSnapshot,
				confidence: lastConfidence,
			}:
			}
			return
		}
		select {
		case <-ctx.Done():
		case out <- transcriptDone{turnIndex: turnIndex, err: fmt.Errorf("stt: transcription ended before producing a final transcript")}:
		}
	}
}

func (c *Coach) runResponse(ctx context.Context, rec TurnRecord, userMessage llm.Message, conversation []llm.Message) responseDone {
	text, err := c.resolveAssistant(ctx, conversation)
	if err != nil {
		return responseDone{turnRecord: rec, userMessage: userMessage, text: text, err: err}
	}
	if strings.TrimSpace(text) == "" {
		return responseDone{turnRecord: rec, userMessage: userMessage}
	}

	if err := c.speakText(ctx, text, true); err != nil {
		return responseDone{turnRecord: rec, userMessage: userMessage, text: text, err: err}
	}

	c.consolef("Coach: %s\n", text)
	return responseDone{turnRecord: rec, userMessage: userMessage, text: text}
}

func (c *Coach) resolveAssistant(ctx context.Context, conversation []llm.Message) (string, error) {
	logger := c.logger()
	messages := append([]llm.Message(nil), conversation...)

	for rounds := 0; rounds < 4; rounds++ {
		text, toolCalls, err := c.collectLLMReply(ctx, messages)
		if err != nil {
			return normalizeAssistantText(text), err
		}
		if len(toolCalls) == 0 {
			return normalizeAssistantText(text), nil
		}

		logger.Printf("coach: assistant requested %d tool call(s)", len(toolCalls))
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   strings.TrimSpace(text),
			ToolCalls: cloneToolCalls(toolCalls),
		})
		for _, call := range toolCalls {
			result, toolErr := c.Tools.Execute(call)
			if toolErr != nil {
				logger.Printf("coach: tool %s failed: %v", call.Function.Name, toolErr)
				if result == "" {
					result = fmt.Sprintf("{\"error\":%q}", toolErr.Error())
				}
			}
			messages = append(messages, llm.Message{
				Role:       "tool",
				Name:       call.Function.Name,
				Content:    result,
				ToolCallID: call.ID,
			})
		}
	}

	return "", fmt.Errorf("coach: tool-call loop exceeded maximum rounds")
}

func normalizeAssistantText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"\u2014", ", ",
		"\u2013", "-",
		"\u2018", "'",
		"\u2019", "'",
		"\u201c", "\"",
		"\u201d", "\"",
	)
	text = replacer.Replace(text)
	text = strings.Join(strings.Fields(text), " ")
	text = strings.ReplaceAll(text, ", ,", ",")
	text = strings.ReplaceAll(text, " ,", ",")
	return text
}

func (c *Coach) collectLLMReply(ctx context.Context, conversation []llm.Message) (string, []llm.ToolCall, error) {
	llmModel := c.LLMModel
	if llmModel == "" {
		llmModel = "mistral-small-latest"
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 80
	}

	req := llm.Request{
		Model:        llmModel,
		SystemPrompt: coachSystemPrompt,
		Messages:     append([]llm.Message(nil), conversation...),
		MaxTokens:    maxTokens,
		Temperature:  c.Temperature,
	}
	if c.Tools != nil {
		req.Tools = c.Tools.Definitions()
	}

	stream, err := c.LLM.Generate(ctx, req)
	if err != nil {
		return "", nil, fmt.Errorf("coach: LLM: %w", err)
	}

	var (
		builder   strings.Builder
		toolCalls []llm.ToolCall
	)
	for {
		select {
		case <-ctx.Done():
			return strings.TrimSpace(builder.String()), cloneToolCalls(toolCalls), ctx.Err()
		case chunk, ok := <-stream:
			if !ok {
				return strings.TrimSpace(builder.String()), cloneToolCalls(toolCalls), nil
			}
			if chunk.Text != "" {
				builder.WriteString(chunk.Text)
			}
			if len(chunk.ToolCalls) > 0 {
				toolCalls = cloneToolCalls(chunk.ToolCalls)
			}
			if chunk.Final {
				return strings.TrimSpace(builder.String()), cloneToolCalls(toolCalls), nil
			}
		}
	}
}

func (c *Coach) finishCalibration(ctx context.Context, samples []pitch.Result) error {
	baseline, message, err := c.buildCalibrationSummary(samples)
	if err != nil {
		return err
	}
	if c.Session != nil {
		if err := c.Session.SaveBaseline(baseline); err != nil {
			return err
		}
	}
	c.consolef("Baseline: %.0f Hz (%s, %s), range %s to %s\n",
		baseline.MedianFreq, baseline.MedianNote, baseline.Name, baseline.LowNote, baseline.HighNote)
	if err := c.speakText(ctx, message, true); err != nil && !errors.Is(err, context.Canceled) {
		c.logger().Printf("coach: baseline summary TTS failed: %v", err)
	}
	return nil
}

func (c *Coach) buildCalibrationSummary(samples []pitch.Result) (pitch.VoiceType, string, error) {
	if len(samples) == 0 {
		return pitch.VoiceType{}, "", fmt.Errorf("no usable pitch samples captured")
	}

	baseline := pitch.ClassifyVoice(samples)
	if baseline.Name == "" || baseline.MedianFreq <= 0 {
		return pitch.VoiceType{}, "", fmt.Errorf("baseline classification unavailable")
	}

	message := fmt.Sprintf(
		"Your natural voice centers around %.0f hertz, %s, in the %s range. Let's work on deepening it.",
		baseline.MedianFreq, baseline.MedianNote, baseline.Name,
	)
	return baseline, message, nil
}

func (c *Coach) streamSpeech(ctx context.Context, text string, onChunk func(tts.Chunk) error) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	ttsModel := c.TTSModel
	if ttsModel == "" {
		ttsModel = "sonic-3"
	}
	lang := c.Language
	if lang == "" {
		lang = "en"
	}

	textCh := make(chan string, 1)
	textCh <- text
	close(textCh)

	stream, err := c.TTS.Synthesize(ctx, tts.Request{
		ModelID:  ttsModel,
		VoiceID:  c.VoiceID,
		Language: lang,
	}, textCh)
	if err != nil {
		return fmt.Errorf("coach: TTS: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-stream:
			if !ok {
				return nil
			}
			if chunk.Done {
				continue
			}
			if onChunk != nil {
				if err := onChunk(chunk); err != nil {
					return err
				}
			}
		}
	}
}

func (c *Coach) speakText(ctx context.Context, text string, announce bool) error {
	if c.Player == nil {
		return fmt.Errorf("coach: playback player is required")
	}

	var (
		playerInput   chan audio.AudioFrame
		playerDone    chan error
		playerStarted bool
	)

	startPlayer := func() {
		if playerStarted {
			return
		}
		playerInput = make(chan audio.AudioFrame, 32)
		playerDone = make(chan error, 1)
		playerStarted = true
		go func() {
			playerDone <- c.Player.Play(ctx, playerInput)
		}()
	}

	firstAudio := true
	err := c.streamSpeech(ctx, text, func(chunk tts.Chunk) error {
		if !playerStarted {
			startPlayer()
		}
		if firstAudio && announce {
			c.consolef("Coach speaking...\n")
			firstAudio = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case playerInput <- chunk.Frame:
			return nil
		}
	})
	if playerStarted {
		close(playerInput)
		if playErr := <-playerDone; playErr != nil && !errors.Is(playErr, context.Canceled) {
			return fmt.Errorf("coach: playback: %w", playErr)
		}
	}
	if err != nil {
		return err
	}
	return nil
}

func (c *Coach) logger() *log.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return log.Default()
}

func (c *Coach) consolef(format string, args ...any) {
	if !c.ConsoleFeed {
		return
	}
	fmt.Printf(format, args...)
}

func cloneToolCalls(in []llm.ToolCall) []llm.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]llm.ToolCall, len(in))
	copy(out, in)
	return out
}

func sendFrame(out chan *audio.AudioFrame, frame *audio.AudioFrame) {
	select {
	case out <- frame:
	default:
		select {
		case <-out:
		default:
		}
		select {
		case out <- frame:
		default:
		}
	}
}
