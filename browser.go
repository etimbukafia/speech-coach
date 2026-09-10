package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/etimbukafia/real-time-voice-pipeline-go/audio"
	"github.com/etimbukafia/real-time-voice-pipeline-go/llm"
	"github.com/etimbukafia/real-time-voice-pipeline-go/pipeline"
	"github.com/etimbukafia/real-time-voice-pipeline-go/tts"

	"github.com/etimbukafia/speech-coach/pitch"
)

var errBrowserSessionEnded = errors.New("browser session ended")

//go:embed web/*
var speechCoachWeb embed.FS

var speechCoachUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin == "" {
			return true
		}
		parsed, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return strings.EqualFold(parsed.Host, r.Host)
	},
}

func runBrowserServer(ctx context.Context, logger *log.Logger, listenAddr string, store *Store) error {
	if store == nil {
		return fmt.Errorf("browser: session store is required")
	}

	webRoot, err := fs.Sub(speechCoachWeb, "web")
	if err != nil {
		return fmt.Errorf("browser: static assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", withCacheHeaders(http.FileServer(http.FS(webRoot))))
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if err := serveBrowserSession(ctx, logger, store, w, r); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("browser: websocket session failed: %v", err)
		}
	})

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Printf("speech-coach browser listening at http://%s", listenAddr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	return err
}

type browserSession struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cleanup func()

	logger  *log.Logger
	store   *Store
	conn    *websocket.Conn
	writeMu sync.Mutex

	mu              sync.Mutex
	coach           *Coach
	pitch           *pitch.Detector
	session         *Session
	clientSessionID string
	started         bool
	busy            bool
	busyDone        chan struct{}
	busyToken       uint64
	closing         bool
	reportSent      bool
	activeTurn      *browserTurn
	pendingClarify  *pendingClarification
	assistantSeq    uint64
	activeAssistant uint64
	assistantCancel context.CancelFunc
	assistantTurn   string
}

type browserTurn struct {
	ctx        context.Context
	cancel     context.CancelFunc
	index      int
	audioIn    chan *audio.AudioFrame
	resultCh   chan transcriptDone
	pitches    *pitchAccum
	pitchBuf   []byte
	closed     bool
	startedAt  time.Time
	frameCount int
	byteCount  int
}

const minServerTurnFrames = 6
const transcriptTurnTimeout = 6 * time.Second

const browserWelcomePrompt = "Hey, I'm Rowan. Let's just talk normally, and I'll coach your voice naturally as we go."

func serveBrowserSession(parent context.Context, logger *log.Logger, store *Store, w http.ResponseWriter, r *http.Request) error {
	logger.Printf("browser: websocket upgrade requested remote=%s origin=%q path=%s", r.RemoteAddr, r.Header.Get("Origin"), r.URL.Path)
	conn, err := speechCoachUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("browser: upgrade websocket: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	bs := &browserSession{
		ctx:    ctx,
		cancel: cancel,
		logger: logger,
		store:  store,
		conn:   conn,
		pitch:  pitch.NewDetector(audio.SampleRate, 75.0, 800.0, 0.15),
	}
	logger.Printf("browser: websocket connected remote=%s", r.RemoteAddr)
	defer bs.close()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || errors.Is(err, context.Canceled) {
				logger.Printf("browser: websocket closing cleanly db_session=%s", bs.dbSessionID())
				return nil
			}
			return fmt.Errorf("browser: read websocket message: %w", err)
		}
		if err := bs.handleClientMessage(data); err != nil {
			if errors.Is(err, errBrowserSessionEnded) {
				return nil
			}
			bs.logger.Printf("browser: client message error: %v", err)
			_ = bs.sendError(err.Error())
		}
	}
}

func (s *browserSession) close() {
	s.logger.Printf("browser: closing session db_session=%s client_session=%s", s.dbSessionID(), s.eventSessionID())
	s.cancel()

	s.mu.Lock()
	turn := s.activeTurn
	s.activeTurn = nil
	s.mu.Unlock()
	if turn != nil {
		turn.cancel()
		if !turn.closed {
			close(turn.audioIn)
		}
	}

	if s.cleanup != nil {
		s.cleanup()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

func (s *browserSession) handleClientMessage(data []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("invalid JSON payload")
	}
	eventType := strings.TrimSpace(asString(payload["type"]))
	if eventType != "client.audio.frame" {
		s.logger.Printf("browser: received client event %s db_session=%s client_session=%s", eventType, s.dbSessionID(), s.eventSessionID())
	}

	switch eventType {
	case "client.session.start":
		return s.handleSessionStart(payload)
	case "client.audio.frame":
		return s.handleAudioFrame(payload)
	case "client.audio.end":
		return s.handleAudioEnd(asString(payload["reason"]))
	case "client.text.input":
		return s.handleTextInput(payload)
	case "client.interrupt":
		return s.handleInterrupt()
	case "client.session.end":
		return s.handleSessionEnd()
	default:
		return fmt.Errorf("unsupported client event %q", asString(payload["type"]))
	}
}

func (s *browserSession) handleSessionStart(payload map[string]any) error {
	clientSessionID := strings.TrimSpace(asString(payload["session_id"]))
	if clientSessionID == "" {
		clientSessionID = time.Now().UTC().Format("20060102T150405.000000000")
	}
	allowResume := asBool(payload["resume"])

	s.mu.Lock()
	alreadyStarted := s.started
	s.started = true
	s.clientSessionID = clientSessionID
	s.mu.Unlock()
	recovered, err := s.ensureFoundation(clientSessionID, allowResume)
	if err != nil {
		return err
	}
	s.logger.Printf("browser: session.start db_session=%s client_session=%s resume=%t already_started=%t", s.session.ID, clientSessionID, allowResume, alreadyStarted)

	if alreadyStarted {
		return s.sendState("ready", map[string]any{
			"phase":         s.currentPhase(),
			"db_session_id": s.session.ID,
		})
	}

	if err := s.sendState("connected", map[string]any{
		"phase":         "conversation",
		"mode":          "browser",
		"db_session_id": s.session.ID,
	}); err != nil {
		return err
	}
	if recovered {
		if err := s.sendRecoverySnapshot(); err != nil {
			return err
		}
		return s.sendState("ready", map[string]any{
			"phase":         "conversation",
			"db_session_id": s.session.ID,
			"recovered":     true,
		})
	}
	if err := s.runAssistantAction("welcome-turn", func(turnCtx context.Context) error {
		return s.sendAssistantSpeech(turnCtx, browserWelcomePrompt, "welcome-turn")
	}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return s.sendState("ready", map[string]any{"phase": "conversation"})
}

func (s *browserSession) ensureFoundation(clientSessionID string, allowResume bool) (bool, error) {
	s.mu.Lock()
	if s.session != nil && s.coach != nil {
		s.mu.Unlock()
		return false, nil
	}
	s.mu.Unlock()

	var (
		session   *Session
		recovered bool
		err       error
	)
	if allowResume {
		session, recovered, err = OpenOrCreateSession(s.store, clientSessionID)
		if err != nil {
			return false, fmt.Errorf("browser: open session: %w", err)
		}
	} else {
		session, err = NewSession(s.store)
		if err != nil {
			return false, fmt.Errorf("browser: create session: %w", err)
		}
	}
	coach, cleanup, err := newCoachFoundation(s.ctx, s.logger, session, false)
	if err != nil {
		return false, fmt.Errorf("browser: create coach foundation: %w", err)
	}

	s.mu.Lock()
	s.session = session
	s.coach = coach
	s.cleanup = cleanup
	s.mu.Unlock()
	s.logger.Printf("browser: session foundation ready db_session=%s recovered=%t", session.ID, recovered)
	return recovered, nil
}

func (s *browserSession) sendRecoverySnapshot() error {
	if s.session == nil {
		return nil
	}
	turns, err := s.session.Store.RecentTurns(s.session.ID, 8)
	if err != nil {
		return fmt.Errorf("browser: recent turns for recovery: %w", err)
	}
	transcript := make([]map[string]any, 0, len(turns)*2)
	for _, turn := range turns {
		if strings.TrimSpace(turn.UserText) != "" {
			transcript = append(transcript, map[string]any{
				"speaker": "user",
				"turn_id": turnID(turn.TurnIndex),
				"text":    turn.UserText,
			})
		}
		if strings.TrimSpace(turn.CoachText) != "" {
			transcript = append(transcript, map[string]any{
				"speaker": "assistant",
				"turn_id": turnID(turn.TurnIndex),
				"text":    turn.CoachText,
			})
		}
	}

	payload := map[string]any{
		"type":          "session.recovered",
		"session_id":    s.eventSessionID(),
		"db_session_id": s.session.ID,
		"phase":         s.currentPhase(),
		"transcript":    transcript,
	}
	if s.session.Baseline != nil {
		payload["baseline"] = map[string]any{
			"median_freq_hz": s.session.Baseline.MedianFreq,
			"median_note":    s.session.Baseline.MedianNote,
			"voice_type":     s.session.Baseline.Name,
			"low_note":       s.session.Baseline.LowNote,
			"high_note":      s.session.Baseline.HighNote,
		}
	}
	if len(turns) > 0 {
		last := turns[len(turns)-1]
		if last.MedianFreq > 0 || last.PitchReport != "" {
			payload["latest_analysis"] = map[string]any{
				"median_freq_hz": last.MedianFreq,
				"median_note":    last.MedianNote,
				"voice_type":     last.VoiceType,
				"low_note":       last.LowNote,
				"high_note":      last.HighNote,
				"pitch_report":   last.PitchReport,
			}
		}
	}
	return s.sendJSON(payload)
}

func (s *browserSession) handleAudioFrame(payload map[string]any) error {
	raw := strings.TrimSpace(asString(payload["audio_b64"]))
	if raw == "" {
		return nil
	}

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return fmt.Errorf("session has not started")
	}
	if s.busy {
		s.mu.Unlock()
		return nil
	}
	turn := s.activeTurn
	startedTurn := false
	if turn == nil {
		var err error
		turn, err = s.startTurnLocked()
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.activeTurn = turn
		startedTurn = true
	}
	s.mu.Unlock()
	if startedTurn {
		s.logger.Printf("browser: turn %d started db_session=%s", turn.index, s.session.ID)
		_ = s.sendState("listening", map[string]any{"turn_id": turnID(turn.index)})
	}

	pcm, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return fmt.Errorf("invalid audio payload")
	}
	if len(pcm) == 0 {
		return nil
	}

	frame := &audio.AudioFrame{
		Data:      pcm,
		Timestamp: time.Now(),
	}
	sendFrame(turn.audioIn, frame)
	turn.frameCount++
	turn.byteCount += len(pcm)

	turn.pitchBuf = append(turn.pitchBuf, pcm...)
	if len(turn.pitchBuf) > 2*audio.FrameSize {
		turn.pitchBuf = turn.pitchBuf[len(turn.pitchBuf)-2*audio.FrameSize:]
	}
	if len(turn.pitchBuf) >= 2*audio.FrameSize {
		result := s.pitch.Detect(turn.pitchBuf)
		if result.MIDINote >= 0 && result.Confidence > 0.5 {
			turn.pitches.Add(result)
		}
	}
	return nil
}

func (s *browserSession) handleAudioEnd(reason string) error {
	s.mu.Lock()
	turn := s.activeTurn
	if turn == nil {
		s.mu.Unlock()
		return nil
	}
	s.activeTurn = nil
	busyToken := s.markBusyLocked()
	if !turn.closed {
		close(turn.audioIn)
		turn.closed = true
	}
	s.mu.Unlock()
	s.logger.Printf("browser: turn %d ended db_session=%s reason=%s frames=%d bytes=%d duration=%s", turn.index, s.session.ID, defaultString(reason, "end_of_turn"), turn.frameCount, turn.byteCount, time.Since(turn.startedAt).Round(time.Millisecond))
	if turn.frameCount < minServerTurnFrames {
		s.logger.Printf("browser: turn %d ignored as too short db_session=%s frames=%d", turn.index, s.session.ID, turn.frameCount)
		turn.cancel()
		s.finishBusyCycle(busyToken)
		return nil
	}

	if err := s.sendState("processing", map[string]any{
		"turn_id": turnID(turn.index),
		"reason":  defaultString(reason, "end_of_turn"),
	}); err != nil {
		return err
	}

	go s.finishTurn(turn, busyToken)
	return nil
}

func (s *browserSession) handleTextInput(payload map[string]any) error {
	text := strings.TrimSpace(asString(payload["text"]))
	if text == "" {
		return nil
	}

	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return nil
	}
	busyToken := s.markBusyLocked()
	index := s.nextTurnIndexLocked()
	s.mu.Unlock()
	s.logger.Printf("browser: text turn %d queued db_session=%s", index, s.session.ID)

	if err := s.sendState("processing", map[string]any{"turn_id": turnID(index)}); err != nil {
		return err
	}

	go s.finishTextTurn(index, text, busyToken)
	return nil
}

func (s *browserSession) handleInterrupt() error {
	s.logger.Printf("browser: interrupt requested db_session=%s", s.session.ID)
	s.mu.Lock()
	turn := s.activeTurn
	s.activeTurn = nil
	busyDone := s.busyDone
	s.busyDone = nil
	s.busy = false
	s.busyToken++
	cancelAssistant := s.assistantCancel
	assistantTurn := s.assistantTurn
	s.assistantCancel = nil
	s.activeAssistant = 0
	s.assistantTurn = ""
	s.pendingClarify = nil
	s.busy = false
	s.mu.Unlock()

	if turn != nil {
		turn.cancel()
		if !turn.closed {
			close(turn.audioIn)
			turn.closed = true
		}
	}
	if busyDone != nil {
		close(busyDone)
	}
	if cancelAssistant != nil {
		s.logger.Printf("browser: canceling assistant action turn=%s db_session=%s", defaultString(assistantTurn, "unknown"), s.session.ID)
		cancelAssistant()
	}
	return s.sendState("ready", map[string]any{"phase": s.currentPhase()})
}

func (s *browserSession) handleSessionEnd() error {
	s.logger.Printf("browser: session.end requested db_session=%s", s.session.ID)
	s.mu.Lock()
	s.closing = true
	turn := s.activeTurn
	busy := s.busy
	busyDone := s.busyDone
	s.activeTurn = nil
	if turn != nil && !turn.closed {
		close(turn.audioIn)
		turn.closed = true
	}
	s.mu.Unlock()
	if turn != nil {
		s.logger.Printf("browser: finalizing active turn %d before shutdown db_session=%s", turn.index, s.session.ID)
		select {
		case result := <-turn.resultCh:
			if result.err == nil && strings.TrimSpace(result.text) != "" {
				s.logger.Printf("browser: recording pending turn %d during shutdown db_session=%s", turn.index, s.session.ID)
				if _, err := s.recordTurn(turn.index, result.text, result.pitches); err != nil {
					s.logger.Printf("browser: failed to record pending turn %d during shutdown db_session=%s err=%v", turn.index, s.session.ID, err)
				}
			} else if result.err != nil {
				s.logger.Printf("browser: pending turn %d dropped during shutdown db_session=%s err=%v", turn.index, s.session.ID, result.err)
			}
		case <-time.After(1200 * time.Millisecond):
			s.logger.Printf("browser: pending turn %d timed out during shutdown db_session=%s", turn.index, s.session.ID)
		case <-s.ctx.Done():
		}
		turn.cancel()
	} else if busy && busyDone != nil {
		s.logger.Printf("browser: waiting for in-flight processing before shutdown db_session=%s", s.session.ID)
		select {
		case <-busyDone:
			s.logger.Printf("browser: in-flight processing drained before shutdown db_session=%s", s.session.ID)
		case <-time.After(1200 * time.Millisecond):
			s.logger.Printf("browser: in-flight processing timed out during shutdown db_session=%s", s.session.ID)
		case <-s.ctx.Done():
		}
	}

	if err := s.sendSessionReport("client_disconnect"); err != nil {
		return err
	}
	s.cancel()
	s.writeMu.Lock()
	_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"), time.Now().Add(2*time.Second))
	s.writeMu.Unlock()
	return errBrowserSessionEnded
}

func (s *browserSession) startTurnLocked() (*browserTurn, error) {
	index := s.nextTurnIndexLocked()
	turnCtx, cancel := context.WithCancel(s.ctx)
	audioIn := make(chan *audio.AudioFrame, 32)
	transcripts, err := s.coach.STT.Transcribe(turnCtx, audioIn)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stt: %w", err)
	}

	turn := &browserTurn{
		ctx:       turnCtx,
		cancel:    cancel,
		index:     index,
		audioIn:   audioIn,
		resultCh:  make(chan transcriptDone, 1),
		pitches:   &pitchAccum{},
		startedAt: time.Now(),
	}
	stabilizer := pipeline.NewTranscriptStabilizer(2)
	commits := stabilizer.Run(turnCtx, transcripts)
	go s.coach.waitForTranscript(turnCtx, turn.index, commits, turn.pitches, turn.resultCh)
	return turn, nil
}

func (s *browserSession) nextTurnIndexLocked() int {
	stats, err := s.session.Store.SessionStats(s.session.ID)
	if err != nil {
		return 1
	}
	return stats.Turns + 1
}

func (s *browserSession) finishTurn(turn *browserTurn, busyToken uint64) {
	defer turn.cancel()

	var result transcriptDone
	select {
	case <-s.ctx.Done():
		s.logger.Printf("browser: turn %d canceled before transcript db_session=%s", turn.index, s.session.ID)
		s.finishBusyCycle(busyToken)
		return
	case <-time.After(transcriptTurnTimeout):
		s.logger.Printf("browser: turn %d transcript timed out db_session=%s timeout=%s", turn.index, s.session.ID, transcriptTurnTimeout)
		_ = s.sendError("Transcription timed out. Please try that again.")
		_ = s.sendState("ready", map[string]any{"phase": s.currentPhase()})
		s.finishBusyCycle(busyToken)
		return
	case result = <-turn.resultCh:
	}

	if result.err != nil {
		s.logger.Printf("browser: turn %d transcript error db_session=%s err=%v", turn.index, s.session.ID, result.err)
		_ = s.sendError(result.err.Error())
		_ = s.sendState("ready", map[string]any{"phase": s.currentPhase()})
		s.finishBusyCycle(busyToken)
		return
	}

	text := strings.TrimSpace(result.text)
	if text == "" {
		s.logger.Printf("browser: turn %d produced empty transcript db_session=%s", turn.index, s.session.ID)
		_ = s.sendState("ready", map[string]any{"phase": s.currentPhase()})
		s.finishBusyCycle(busyToken)
		return
	}

	if s.isClosing() {
		s.logger.Printf("browser: turn %d transcript finalized during shutdown db_session=%s", turn.index, s.session.ID)
		if _, err := s.recordTurn(turn.index, text, result.pitches); err != nil {
			_ = s.sendError(err.Error())
		}
		s.finishBusyCycle(busyToken)
		return
	}

	if err := s.handleResolvedUserTurn(turn.index, text, result.pitches, result.confidence); err != nil {
		_ = s.sendError(err.Error())
	}
	s.finishBusyCycle(busyToken)
}

func (s *browserSession) finishCoachingTurn(index int, userText string, samples []pitch.Result) {
	s.logger.Printf("browser: coaching turn %d start db_session=%s", index, s.session.ID)
	rec, err := s.recordTurn(index, userText, samples)
	if err != nil {
		_ = s.sendError(err.Error())
		return
	}

	_ = s.sendTurnAnalysis(index, rec)

	userMessage := llm.Message{
		Role:    "user",
		Content: userText + "\n\n[PITCH ANALYSIS] " + rec.PitchReport,
	}
	messages := []llm.Message{{Role: "system", Content: "Session context unavailable."}}
	if built, err := s.session.BuildMessages(8); err == nil && len(built) > 0 {
		messages = built
	}
	messages = append(messages, userMessage)

	var text string
	err = s.runAssistantAction(turnID(index), func(turnCtx context.Context) error {
		var resolveErr error
		text, resolveErr = s.coach.resolveAssistant(turnCtx, messages)
		if resolveErr != nil {
			return resolveErr
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return s.sendAssistantSpeech(turnCtx, text, turnID(index))
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			s.logger.Printf("browser: coaching turn %d canceled db_session=%s", index, s.session.ID)
			return
		}
		s.logger.Printf("browser: coaching turn %d LLM error db_session=%s err=%v", index, s.session.ID, err)
		_ = s.sendError(err.Error())
		return
	}
	if text == "" {
		s.logger.Printf("browser: coaching turn %d produced empty assistant reply db_session=%s", index, s.session.ID)
		return
	}

	rec.CoachText = text
	if err := s.session.SaveTurn(rec); err != nil {
		_ = s.sendError(err.Error())
		return
	}
	s.session.AppendConversation(userMessage, llm.Message{
		Role:    "assistant",
		Content: text,
	})
	if err := s.session.RefreshMemory(); err != nil {
		s.logger.Printf("browser: failed to refresh session memory db_session=%s err=%v", s.session.ID, err)
	}
	s.logger.Printf("browser: coaching turn %d assistant reply ready db_session=%s text=%q", index, s.session.ID, text)
}

func (s *browserSession) recordTurn(index int, userText string, samples []pitch.Result) (TurnRecord, error) {
	s.maybeLearnBaseline(samples)
	rec := BuildTurnRecord(index, userText, samples)
	if err := s.session.SaveTurn(rec); err != nil {
		return TurnRecord{}, err
	}
	return rec, nil
}

func (s *browserSession) finishTextTurn(index int, userText string, busyToken uint64) {
	defer s.finishBusyCycle(busyToken)
	if s.isClosing() {
		if _, err := s.recordTurn(index, userText, nil); err != nil {
			_ = s.sendError(err.Error())
		}
		return
	}
	if err := s.handleResolvedUserTurn(index, userText, nil, 1.0); err != nil {
		_ = s.sendError(err.Error())
	}
}

func (s *browserSession) handleResolvedUserTurn(index int, userText string, samples []pitch.Result, confidence float64) error {
	if resolved, handled, err := s.resolvePendingClarification(strings.TrimSpace(userText)); handled {
		if err != nil {
			return err
		}
		if resolved == nil {
			return nil
		}
		return s.finalizeUserTurn(resolved.TurnIndex, resolved.Transcript, resolved.Pitches)
	}

	if shouldClarifyTranscript(userText, confidence) {
		s.maybeLearnBaseline(samples)
		rec := BuildTurnRecord(index, strings.TrimSpace(userText), samples)
		_ = s.sendJSON(map[string]any{
			"type":       "user.transcript.provisional",
			"session_id": s.eventSessionID(),
			"turn_id":    turnID(index),
			"text":       strings.TrimSpace(userText),
		})
		_ = s.sendTurnAnalysis(index, rec)
		s.mu.Lock()
		s.pendingClarify = &pendingClarification{
			TurnIndex:  index,
			Transcript: strings.TrimSpace(userText),
			Pitches:    append([]pitch.Result(nil), samples...),
			Confidence: confidence,
		}
		s.mu.Unlock()
		s.logger.Printf("browser: clarification requested turn=%d db_session=%s text=%q confidence=%.2f", index, s.session.ID, strings.TrimSpace(userText), confidence)
		return s.sendAssistantSpeech(s.ctx, clarificationPrompt(userText), fmt.Sprintf("clarify-%03d", index))
	}

	return s.finalizeUserTurn(index, userText, samples)
}

func (s *browserSession) resolvePendingClarification(userText string) (*pendingClarification, bool, error) {
	s.mu.Lock()
	pending := s.pendingClarify
	s.mu.Unlock()
	if pending == nil {
		return nil, false, nil
	}

	if pending.AwaitingCorrection {
		s.mu.Lock()
		s.pendingClarify = nil
		s.mu.Unlock()
		pending.Transcript = strings.TrimSpace(userText)
		return pending, true, nil
	}

	if isAffirmativeClarification(userText) {
		s.mu.Lock()
		s.pendingClarify = nil
		s.mu.Unlock()
		return pending, true, nil
	}

	if isNegativeClarification(userText) {
		s.mu.Lock()
		if s.pendingClarify != nil {
			s.pendingClarify.AwaitingCorrection = true
		}
		s.mu.Unlock()
		return nil, true, s.sendAssistantSpeech(s.ctx, repeatPrompt(), fmt.Sprintf("clarify-repeat-%03d", pending.TurnIndex))
	}

	s.mu.Lock()
	s.pendingClarify = nil
	s.mu.Unlock()
	pending.Transcript = strings.TrimSpace(userText)
	return pending, true, nil
}

func (s *browserSession) finalizeUserTurn(index int, userText string, samples []pitch.Result) error {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return nil
	}
	if err := s.sendJSON(map[string]any{
		"type":       "user.transcript.final",
		"session_id": s.eventSessionID(),
		"turn_id":    turnID(index),
		"text":       userText,
	}); err != nil {
		return err
	}
	s.logger.Printf("browser: turn %d transcript final db_session=%s text=%q", index, s.session.ID, userText)
	s.finishCoachingTurn(index, userText, samples)
	return nil
}

func (s *browserSession) sendTurnAnalysis(index int, rec TurnRecord) error {
	return s.sendJSON(map[string]any{
		"type":       "turn.analysis",
		"session_id": s.eventSessionID(),
		"turn_id":    turnID(index),
		"analysis": map[string]any{
			"median_freq_hz": rec.MedianFreq,
			"median_note":    rec.MedianNote,
			"voice_type":     rec.VoiceType,
			"low_note":       rec.LowNote,
			"high_note":      rec.HighNote,
			"pitch_report":   rec.PitchReport,
		},
	})
}

func (s *browserSession) finishBusyCycle(token uint64) {
	s.mu.Lock()
	if token != s.busyToken {
		s.mu.Unlock()
		return
	}
	s.busy = false
	busyDone := s.busyDone
	s.busyDone = nil
	s.mu.Unlock()
	if busyDone != nil {
		close(busyDone)
	}
	s.logger.Printf("browser: session ready db_session=%s phase=%s", s.session.ID, s.currentPhase())
	_ = s.sendState("ready", map[string]any{"phase": s.currentPhase()})
}

func (s *browserSession) sendSessionReport(reason string) error {
	s.mu.Lock()
	if s.reportSent {
		s.mu.Unlock()
		return nil
	}
	s.reportSent = true
	s.mu.Unlock()

	report, err := s.session.BuildReport()
	if err != nil {
		s.logger.Printf("browser: failed to build session report db_session=%s err=%v", s.session.ID, err)
		return err
	}
	s.logger.Printf("browser: sending session report db_session=%s reason=%s turns=%d", s.session.ID, reason, report.Turns)
	printSessionReport(report)
	payload := map[string]any{
		"type":                    "session.report",
		"session_id":              s.eventSessionID(),
		"db_session_id":           report.SessionID,
		"reason":                  reason,
		"started_at":              report.StartedAt,
		"ended_at":                report.EndedAt,
		"duration":                report.Duration,
		"turns":                   report.Turns,
		"average_pitch_hz":        report.AveragePitchHz,
		"trend":                   report.Trend,
		"previous_session_avg_hz": report.PreviousSessionAvg,
	}
	if report.Baseline != nil {
		payload["baseline"] = map[string]any{
			"median_freq_hz": report.Baseline.MedianFreq,
			"median_note":    report.Baseline.MedianNote,
			"voice_type":     report.Baseline.Name,
			"low_note":       report.Baseline.LowNote,
			"high_note":      report.Baseline.HighNote,
		}
	}
	if report.BestTurn != nil {
		payload["best_turn"] = map[string]any{
			"turn":           report.BestTurn.TurnIndex,
			"median_freq_hz": report.BestTurn.MedianFreq,
			"median_note":    report.BestTurn.MedianNote,
			"voice_type":     report.BestTurn.VoiceType,
		}
	}
	if len(report.RecentTurns) > 0 {
		recent := make([]map[string]any, 0, len(report.RecentTurns))
		for _, turn := range report.RecentTurns {
			recent = append(recent, map[string]any{
				"turn":           turn.TurnIndex,
				"median_freq_hz": turn.MedianFreq,
				"median_note":    turn.MedianNote,
				"voice_type":     turn.VoiceType,
				"pitch_report":   turn.PitchReport,
			})
		}
		payload["recent_turns"] = recent
	}
	return s.sendJSON(payload)
}

func (s *browserSession) maybeLearnBaseline(samples []pitch.Result) {
	if len(samples) == 0 {
		return
	}
	s.session.mu.Lock()
	hasBaseline := s.session.Baseline != nil && s.session.Baseline.Name != ""
	s.session.mu.Unlock()
	if hasBaseline {
		return
	}

	baseline, _, err := s.coach.buildCalibrationSummary(samples)
	if err != nil {
		s.logger.Printf("browser: passive baseline learning skipped db_session=%s err=%v", s.session.ID, err)
		return
	}
	if err := s.session.SaveBaseline(baseline); err != nil {
		s.logger.Printf("browser: passive baseline save failed db_session=%s err=%v", s.session.ID, err)
		return
	}
	s.logger.Printf("browser: passive baseline learned db_session=%s baseline=%.0fHz %s %s", s.session.ID, baseline.MedianFreq, baseline.MedianNote, baseline.Name)
	_ = s.sendJSON(map[string]any{
		"type":          "baseline.ready",
		"session_id":    s.eventSessionID(),
		"db_session_id": s.session.ID,
		"source":        "passive",
		"baseline": map[string]any{
			"median_freq_hz": baseline.MedianFreq,
			"median_note":    baseline.MedianNote,
			"voice_type":     baseline.Name,
			"low_note":       baseline.LowNote,
			"high_note":      baseline.HighNote,
		},
	})
}

func (s *browserSession) sendAssistantSpeech(ctx context.Context, text, turn string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if err := s.sendJSON(map[string]any{
		"type":       "assistant.text.final",
		"session_id": s.eventSessionID(),
		"turn_id":    turn,
		"text":       text,
	}); err != nil {
		return err
	}
	if err := s.sendState("speaking", map[string]any{"turn_id": turn}); err != nil {
		return err
	}
	s.logger.Printf("browser: assistant speaking turn=%s db_session=%s text_len=%d", turn, s.session.ID, len(text))

	chunkIndex := 0
	if err := s.coach.streamSpeech(ctx, text, func(chunk tts.Chunk) error {
		if err := s.sendJSON(map[string]any{
			"type":           "assistant.audio.chunk",
			"session_id":     s.eventSessionID(),
			"turn_id":        turn,
			"audio_b64":      base64.StdEncoding.EncodeToString(chunk.Frame.Data),
			"chunk_index":    chunkIndex,
			"sample_rate_hz": audio.SampleRate,
			"encoding":       "pcm_s16le",
			"is_final":       false,
		}); err != nil {
			return err
		}
		chunkIndex++
		return nil
	}); err != nil {
		s.logger.Printf("browser: assistant TTS stream failed turn=%s db_session=%s err=%v", turn, s.session.ID, err)
		return err
	}
	s.logger.Printf("browser: assistant speaking done turn=%s db_session=%s chunks=%d", turn, s.session.ID, chunkIndex)
	return s.sendJSON(map[string]any{
		"type":           "assistant.audio.chunk",
		"session_id":     s.eventSessionID(),
		"turn_id":        turn,
		"audio_b64":      "",
		"chunk_index":    chunkIndex,
		"sample_rate_hz": audio.SampleRate,
		"encoding":       "pcm_s16le",
		"is_final":       true,
	})
}

func (s *browserSession) sendState(state string, extra map[string]any) error {
	payload := map[string]any{
		"type":          "session.state",
		"session_id":    s.eventSessionID(),
		"db_session_id": s.session.ID,
		"state":         state,
	}
	for key, value := range extra {
		payload[key] = value
	}
	return s.sendJSON(payload)
}

func (s *browserSession) sendError(message string) error {
	s.logger.Printf("browser: sending session.error db_session=%s message=%q", s.session.ID, strings.TrimSpace(message))
	return s.sendJSON(map[string]any{
		"type":       "session.error",
		"session_id": s.eventSessionID(),
		"message":    strings.TrimSpace(message),
	})
}

func (s *browserSession) sendJSON(payload map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.WriteJSON(payload); err != nil {
		s.logger.Printf("browser: sendJSON failed db_session=%s type=%v err=%v", s.session.ID, payload["type"], err)
		return err
	}
	return nil
}

func (s *browserSession) eventSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientSessionID != "" {
		return s.clientSessionID
	}
	if s.session != nil {
		return s.session.ID
	}
	return ""
}

func (s *browserSession) dbSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		return s.session.ID
	}
	return ""
}

func (s *browserSession) currentPhase() string {
	return "conversation"
}

func (s *browserSession) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *browserSession) markBusyLocked() uint64 {
	s.busyToken++
	s.busy = true
	s.busyDone = make(chan struct{})
	return s.busyToken
}

func (s *browserSession) runAssistantAction(turn string, fn func(context.Context) error) error {
	actionCtx, seq := s.beginAssistantAction(turn)
	defer s.endAssistantAction(seq)
	return fn(actionCtx)
}

func (s *browserSession) beginAssistantAction(turn string) (context.Context, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assistantSeq++
	seq := s.assistantSeq
	actionCtx, cancel := context.WithCancel(s.ctx)
	s.assistantCancel = cancel
	s.activeAssistant = seq
	s.assistantTurn = turn
	return actionCtx, seq
}

func (s *browserSession) endAssistantAction(seq uint64) {
	s.mu.Lock()
	if s.activeAssistant != seq {
		s.mu.Unlock()
		return
	}
	cancel := s.assistantCancel
	s.assistantCancel = nil
	s.activeAssistant = 0
	s.assistantTurn = ""
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func turnID(index int) string {
	return fmt.Sprintf("turn-%03d", index)
}

func asString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func defaultString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func asBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		normalized := strings.TrimSpace(strings.ToLower(typed))
		return normalized == "1" || normalized == "true" || normalized == "yes"
	case float64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return false
	}
}

func withCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/index.html"):
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}
