package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/etimbukafia/real-time-voice-pipeline-go/llm"

	"github.com/etimbukafia/speech-coach/pitch"
)

type Session struct {
	mu             sync.Mutex
	ID             string
	StartedAt      time.Time
	Store          *Store
	Baseline       *pitch.VoiceType
	Memory         SessionMemory
	recentMessages []llm.Message
}

type SessionReport struct {
	SessionID          string
	StartedAt          time.Time
	EndedAt            time.Time
	Duration           string
	Baseline           *pitch.VoiceType
	Turns              int
	AveragePitchHz     float64
	Trend              string
	BestTurn           *TurnRecord
	PreviousSessionAvg float64
	RecentTurns        []TurnRecord
}

type coachingSignals struct {
	PitchTrend           string
	ConsecutiveHighTurns int
	TurnsSinceCorrection int
	TurnsSincePraise     int
	TurnsSinceSelfCheck  int
	TurnsSinceCarryover  int
	PracticeContext      string
	CarryoverSuggestion  string
}

type SessionMemory struct {
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
	UserGoal        string    `json:"user_goal,omitempty"`
	PracticeContext string    `json:"practice_context,omitempty"`
	RecurringIssues []string  `json:"recurring_issues,omitempty"`
	RecentWins      []string  `json:"recent_wins,omitempty"`
	LastCue         string    `json:"last_cue,omitempty"`
	WorkingCue      string    `json:"working_cue,omitempty"`
	CarryoverFocus  string    `json:"carryover_focus,omitempty"`
}

func NewSession(store *Store) (*Session, error) {
	if store == nil {
		return nil, fmt.Errorf("session: store is required")
	}

	now := time.Now().UTC()
	id := now.Format("20060102T150405.000000000")
	session := &Session{
		ID:        id,
		StartedAt: now,
		Store:     store,
	}
	if err := store.CreateSession(id); err != nil {
		return nil, err
	}
	return session, nil
}

func OpenOrCreateSession(store *Store, id string) (*Session, bool, error) {
	if store == nil {
		return nil, false, fmt.Errorf("session: store is required")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false, fmt.Errorf("session: id is required")
	}

	loaded, err := store.LoadSession(id)
	if err == nil {
		session := &Session{
			ID:        loaded.ID,
			StartedAt: loaded.StartedAt,
			Store:     store,
		}
		if loaded.BaselineType != "" && loaded.BaselineFreq > 0 {
			session.Baseline = &pitch.VoiceType{
				MedianFreq: loaded.BaselineFreq,
				MedianNote: loaded.BaselineNote,
				Name:       loaded.BaselineType,
				LowNote:    loaded.BaselineLow,
				HighNote:   loaded.BaselineHigh,
			}
		}
		if loaded.MemoryJSON != "" {
			memory, decodeErr := decodeSessionMemory(loaded.MemoryJSON)
			if decodeErr != nil {
				return nil, false, fmt.Errorf("session: load memory: %w", decodeErr)
			}
			session.Memory = memory
		}
		if turns, turnErr := store.RecentTurns(id, 8); turnErr == nil {
			for _, turn := range turns {
				if strings.TrimSpace(turn.UserText) != "" {
					session.recentMessages = append(session.recentMessages, llm.Message{
						Role:    "user",
						Content: turn.UserText + "\n\n[PITCH ANALYSIS] " + turn.PitchReport,
					})
				}
				if strings.TrimSpace(turn.CoachText) != "" {
					session.recentMessages = append(session.recentMessages, llm.Message{
						Role:    "assistant",
						Content: turn.CoachText,
					})
				}
			}
		}
		return session, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	session := &Session{
		ID:        id,
		StartedAt: time.Now().UTC(),
		Store:     store,
	}
	if err := store.CreateSession(id); err != nil {
		return nil, false, err
	}
	return session, false, nil
}

func (s *Session) SaveBaseline(v pitch.VoiceType) error {
	s.mu.Lock()
	copyValue := v
	s.Baseline = &copyValue
	s.mu.Unlock()
	return s.Store.SaveBaseline(s.ID, v)
}

func (s *Session) SaveTurn(rec TurnRecord) error {
	return s.Store.SaveTurn(s.ID, rec)
}

func (s *Session) AppendConversation(user, assistant llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recentMessages = append(s.recentMessages, user, assistant)
	if len(s.recentMessages) > 16 {
		s.recentMessages = append([]llm.Message(nil), s.recentMessages[len(s.recentMessages)-16:]...)
	}
}

func (s *Session) RefreshMemory() error {
	recentTurns, err := s.Store.RecentTurns(s.ID, 6)
	if err != nil {
		return err
	}

	s.mu.Lock()
	baseline := s.Baseline
	recent := append([]llm.Message(nil), s.recentMessages...)
	existing := s.Memory
	s.mu.Unlock()

	signals := buildCoachingSignals(baseline, recentTurns, recent)
	updated := buildSessionMemory(existing, baseline, recentTurns, recent, signals)
	if sessionMemoryEqual(existing, updated) {
		return nil
	}
	updated.UpdatedAt = time.Now().UTC()

	s.mu.Lock()
	s.Memory = updated
	s.mu.Unlock()

	return s.Store.SaveSessionMemory(s.ID, updated)
}

func (s *Session) BuildMessages(maxRecentPairs int) ([]llm.Message, error) {
	stats, err := s.Store.SessionStats(s.ID)
	if err != nil {
		return nil, err
	}
	previous, err := s.Store.PreviousSessions(s.ID, 1)
	if err != nil {
		return nil, err
	}
	recentTurns, err := s.Store.RecentTurns(s.ID, 6)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	baseline := s.Baseline
	memory := s.Memory
	recent := append([]llm.Message(nil), s.recentMessages...)
	startedAt := s.StartedAt
	s.mu.Unlock()

	if maxRecentPairs > 0 {
		maxMessages := maxRecentPairs * 2
		if len(recent) > maxMessages {
			recent = recent[len(recent)-maxMessages:]
		}
	}

	signals := buildCoachingSignals(baseline, recentTurns, recent)
	memory = buildSessionMemory(memory, baseline, recentTurns, recent, signals)
	system := llm.Message{
		Role:    "system",
		Content: buildSessionContext(startedAt, baseline, stats, previous, memory, signals),
	}
	out := make([]llm.Message, 0, len(recent)+1)
	out = append(out, system)
	out = append(out, recent...)
	return out, nil
}

func (s *Session) PrintReport() error {
	report, err := s.BuildReport()
	if err != nil {
		return err
	}
	printSessionReport(report)
	return nil
}

func (s *Session) BuildReport() (SessionReport, error) {
	stats, err := s.Store.SessionStats(s.ID)
	if err != nil {
		return SessionReport{}, err
	}
	previous, err := s.Store.PreviousSessions(s.ID, 1)
	if err != nil {
		return SessionReport{}, err
	}
	recent, err := s.Store.RecentTurns(s.ID, 5)
	if err != nil {
		return SessionReport{}, err
	}

	s.mu.Lock()
	baseline := s.Baseline
	var baselineCopy *pitch.VoiceType
	if baseline != nil {
		copyValue := *baseline
		baselineCopy = &copyValue
	}
	s.mu.Unlock()

	report := SessionReport{
		SessionID:   s.ID,
		StartedAt:   s.StartedAt,
		EndedAt:     time.Now().UTC(),
		Duration:    humanizeSince(s.StartedAt),
		Baseline:    baselineCopy,
		Turns:       stats.Turns,
		RecentTurns: recent,
	}
	if stats.AvgPitch > 0 {
		report.AveragePitchHz = stats.AvgPitch
	}
	if stats.EarlyAvg > 0 && stats.RecentAvg > 0 {
		report.Trend = describeTrend(stats.EarlyAvg, stats.RecentAvg)
	}
	if stats.HasBestTurn {
		best := stats.BestTurn
		report.BestTurn = &best
	}
	if len(previous) > 0 && previous[0].AvgPitch > 0 && stats.AvgPitch > 0 {
		report.PreviousSessionAvg = previous[0].AvgPitch
	}

	return report, nil
}

func printSessionReport(report SessionReport) {
	fmt.Println()
	fmt.Println("Session report")
	fmt.Println("--------------")
	if report.Baseline != nil && report.Baseline.Name != "" {
		fmt.Printf("Baseline: %.0f Hz (%s, %s), range %s to %s\n",
			report.Baseline.MedianFreq, report.Baseline.MedianNote, report.Baseline.Name, report.Baseline.LowNote, report.Baseline.HighNote)
	}
	fmt.Printf("Turns: %d\n", report.Turns)
	if report.AveragePitchHz > 0 {
		fmt.Printf("Average pitch: %.0f Hz\n", report.AveragePitchHz)
	}
	if report.Trend != "" {
		fmt.Printf("Trend: %s\n", report.Trend)
	}
	if report.BestTurn != nil {
		fmt.Printf("Best turn: #%d at %.0f Hz (%s)\n",
			report.BestTurn.TurnIndex, report.BestTurn.MedianFreq, report.BestTurn.MedianNote)
	}
	if report.PreviousSessionAvg > 0 && report.AveragePitchHz > 0 {
		fmt.Printf("Previous session avg: %.0f Hz\n", report.PreviousSessionAvg)
	}
	if len(report.RecentTurns) > 0 {
		fmt.Println("Recent turns:")
		for _, turn := range report.RecentTurns {
			if turn.MedianFreq > 0 {
				fmt.Printf("  #%d  %.0f Hz (%s, %s)\n",
					turn.TurnIndex, turn.MedianFreq, turn.MedianNote, turn.VoiceType)
				continue
			}
			fmt.Printf("  #%d  no pitch detected\n", turn.TurnIndex)
		}
	}
}

func buildSessionContext(startedAt time.Time, baseline *pitch.VoiceType, stats SessionStats, previous []SessionSummary, memory SessionMemory, signals coachingSignals) string {
	var parts []string
	parts = append(parts, "Live speech coaching session.")
	parts = append(parts, fmt.Sprintf("Session snapshot: started %s ago; turns=%d.", humanizeSince(startedAt), stats.Turns))

	if baseline != nil && baseline.Name != "" {
		parts = append(parts, fmt.Sprintf(
			"Baseline: %.0f Hz (%s, %s); comfortable range %s to %s.",
			baseline.MedianFreq, baseline.MedianNote, baseline.Name, baseline.LowNote, baseline.HighNote,
		))
	}
	if stats.AvgPitch > 0 {
		parts = append(parts, fmt.Sprintf("Pitch snapshot: session_avg=%.0f Hz.", stats.AvgPitch))
	}
	if stats.EarlyAvg > 0 && stats.RecentAvg > 0 {
		parts = append(parts, fmt.Sprintf("Trend: %s.", describeTrend(stats.EarlyAvg, stats.RecentAvg)))
	}
	if stats.HasBestTurn {
		parts = append(parts, fmt.Sprintf(
			"Best turn: #%d at %.0f Hz (%s).",
			stats.BestTurn.TurnIndex, stats.BestTurn.MedianFreq, stats.BestTurn.MedianNote,
		))
	}
	if len(previous) > 0 && previous[0].AvgPitch > 0 {
		parts = append(parts, fmt.Sprintf(
			"Previous session average: %.0f Hz across %d turns.",
			previous[0].AvgPitch, previous[0].Turns,
		))
	}

	parts = append(parts, "Conversation policy: stay natural and user-led; do not correct every turn.")
	parts = append(parts, "Coaching priorities: favor vocal ease, resonance, breath support, pace, and steadiness over forcing pitch lower.")
	parts = append(parts, fmt.Sprintf(
		"Session memory: goal=%q; context=%q; recurring_issues=%s; recent_wins=%s; last_cue=%q; working_cue=%q; carryover_focus=%q.",
		fallbackString(memory.UserGoal, "develop a deeper, steadier speaking voice in natural conversation"),
		fallbackString(memory.PracticeContext, "general conversation practice"),
		joinQuoted(memory.RecurringIssues),
		joinQuoted(memory.RecentWins),
		memory.LastCue,
		memory.WorkingCue,
		memory.CarryoverFocus,
	))
	parts = append(parts, fmt.Sprintf(
		"Coaching cadence: pitch_trend=%s; consecutive_high_turns=%d; turns_since_correction=%d; turns_since_praise=%d; turns_since_self_check=%d; turns_since_carryover=%d.",
		signals.PitchTrend, signals.ConsecutiveHighTurns, signals.TurnsSinceCorrection, signals.TurnsSincePraise, signals.TurnsSinceSelfCheck, signals.TurnsSinceCarryover,
	))
	if signals.TurnsSinceCorrection == 0 {
		parts = append(parts, "The last coach turn already gave a corrective cue. Avoid stacking another unless the current turn clearly repeats the problem or sounds strained.")
	} else if signals.ConsecutiveHighTurns >= 2 {
		parts = append(parts, "Recent turns have stayed high. If the current turn is also high or strained, give one brief corrective cue and stop there.")
	}
	if signals.TurnsSincePraise >= 2 {
		parts = append(parts, "If the current turn is clearly better, acknowledge the improvement briefly.")
	}
	if memory.WorkingCue != "" && signals.PitchTrend != "trending higher" {
		parts = append(parts, fmt.Sprintf("If the user needs a cue, prefer reusing the working cue: %q.", memory.WorkingCue))
	}
	if signals.TurnsSinceSelfCheck >= 3 {
		parts = append(parts, "When the user shows a noticeable shift or easier production, consider one brief self-monitoring question such as asking whether it felt easier, steadier, or lower. Do not ask this every turn.")
	}
	if stats.Turns >= 3 && signals.TurnsSinceCarryover >= 3 {
		parts = append(parts, fmt.Sprintf("If the reply naturally closes a stretch of conversation, you may offer one tiny real-world carryover task. Suggested carryover: %s.", signals.CarryoverSuggestion))
	} else {
		parts = append(parts, "Do not assign homework every turn. Only offer carryover occasionally and keep it to one small real-world task.")
	}
	parts = append(parts, "Use the hidden pitch data and prior session history to decide whether to simply converse, briefly praise, or give one concise cue.")

	return strings.Join(parts, " ")
}

func buildSessionMemory(existing SessionMemory, baseline *pitch.VoiceType, recentTurns []TurnRecord, recentMessages []llm.Message, signals coachingSignals) SessionMemory {
	memory := existing

	if goal := inferUserGoal(recentTurns, signals.PracticeContext); goal != "" {
		memory.UserGoal = goal
	} else if strings.TrimSpace(memory.UserGoal) == "" {
		memory.UserGoal = defaultUserGoal(signals.PracticeContext)
	}
	memory.PracticeContext = signals.PracticeContext
	memory.CarryoverFocus = signals.CarryoverSuggestion

	issues := inferRecurringIssues(baseline, recentTurns, signals)
	if len(issues) == 0 {
		issues = memory.RecurringIssues
	}
	memory.RecurringIssues = clampStrings(issues, 2)

	wins := inferRecentWins(baseline, recentTurns, signals)
	if len(wins) == 0 {
		wins = memory.RecentWins
	}
	memory.RecentWins = clampStrings(wins, 2)

	if cue := findLastCorrectiveCue(recentMessages); cue != "" {
		memory.LastCue = cue
	}
	memory.WorkingCue = inferWorkingCue(memory.LastCue, memory.WorkingCue, signals)

	return normalizeSessionMemory(memory)
}

func buildCoachingSignals(baseline *pitch.VoiceType, recentTurns []TurnRecord, recentMessages []llm.Message) coachingSignals {
	signals := coachingSignals{
		PitchTrend:           "not enough recent pitch data",
		PracticeContext:      inferPracticeContext(recentTurns),
		TurnsSincePraise:     countTurnsSinceAssistantFeedback(recentMessages, isPraiseMessage),
		TurnsSinceCorrection: countTurnsSinceAssistantFeedback(recentMessages, isCorrectiveMessage),
		TurnsSinceSelfCheck:  countTurnsSinceAssistantFeedback(recentMessages, isSelfMonitoringMessage),
		TurnsSinceCarryover:  countTurnsSinceAssistantFeedback(recentMessages, isCarryoverMessage),
	}
	signals.CarryoverSuggestion = buildCarryoverSuggestion(signals.PracticeContext)

	var pitchTurns []TurnRecord
	for _, turn := range recentTurns {
		if turn.MedianFreq > 0 {
			pitchTurns = append(pitchTurns, turn)
		}
	}
	if len(pitchTurns) >= 2 {
		half := len(pitchTurns) / 2
		if half == 0 {
			half = 1
		}
		early := averagePitch(pitchTurns[:half])
		recent := averagePitch(pitchTurns[half:])
		switch {
		case recent <= early-8:
			signals.PitchTrend = "trending lower"
		case recent >= early+8:
			signals.PitchTrend = "trending higher"
		default:
			signals.PitchTrend = "holding fairly steady"
		}
	}

	if baseline != nil && baseline.MedianFreq > 0 {
		highThreshold := baseline.MedianFreq + 8
		for i := len(recentTurns) - 1; i >= 0; i-- {
			turn := recentTurns[i]
			if turn.MedianFreq <= 0 {
				break
			}
			if turn.MedianFreq > highThreshold {
				signals.ConsecutiveHighTurns++
				continue
			}
			break
		}
	}

	return signals
}

func inferUserGoal(recentTurns []TurnRecord, practiceContext string) string {
	for i := len(recentTurns) - 1; i >= 0; i-- {
		text := strings.ToLower(strings.TrimSpace(recentTurns[i].UserText))
		if text == "" {
			continue
		}
		switch {
		case strings.Contains(text, "interview"):
			return "sound grounded and steady in interviews"
		case strings.Contains(text, "presentation") || strings.Contains(text, "speech") || strings.Contains(text, "talk") || strings.Contains(text, "present"):
			return "carry a deeper, steadier voice in presentations"
		case strings.Contains(text, "phone") || strings.Contains(text, "call") || strings.Contains(text, "voicemail"):
			return "keep a calm, grounded voice on calls"
		case strings.Contains(text, "meeting") || strings.Contains(text, "manager") || strings.Contains(text, "coworker") || strings.Contains(text, "client"):
			return "sound steady and grounded in work conversations"
		case strings.Contains(text, "story"):
			return "tell stories with a steadier, more resonant voice"
		case strings.Contains(text, "deep") || strings.Contains(text, "deeper") || strings.Contains(text, "lower"):
			return "build a deeper speaking voice"
		case strings.Contains(text, "steady") || strings.Contains(text, "grounded") || strings.Contains(text, "calm"):
			return "sound steadier and more grounded"
		}
	}
	return defaultUserGoal(practiceContext)
}

func defaultUserGoal(practiceContext string) string {
	switch practiceContext {
	case "job interview practice":
		return "sound grounded and steady in interviews"
	case "work conversation practice":
		return "sound steady and grounded in work conversations"
	case "presentation practice":
		return "carry a deeper, steadier voice in presentations"
	case "phone call practice":
		return "keep a calm, grounded voice on calls"
	default:
		return "develop a deeper, steadier speaking voice in natural conversation"
	}
}

func inferRecurringIssues(baseline *pitch.VoiceType, recentTurns []TurnRecord, signals coachingSignals) []string {
	var issues []string
	switch {
	case signals.ConsecutiveHighTurns >= 2:
		issues = append(issues, "pitch drifts high across consecutive turns")
	case signals.PitchTrend == "trending higher":
		issues = append(issues, "recent pitch trend is rising")
	}
	if baseline != nil && baseline.MedianFreq > 0 && len(recentTurns) > 0 {
		last := recentTurns[len(recentTurns)-1]
		if last.MedianFreq > baseline.MedianFreq+8 {
			issues = append(issues, "staying near the lower target is still inconsistent")
		}
		if strings.Contains(last.PitchReport, "Voice drifted UP") {
			issues = append(issues, "sentence endings are lifting upward")
		}
	}
	return clampStrings(issues, 2)
}

func inferRecentWins(baseline *pitch.VoiceType, recentTurns []TurnRecord, signals coachingSignals) []string {
	var wins []string
	if signals.PitchTrend == "trending lower" {
		wins = append(wins, "recent turns are landing lower")
	}
	if signals.TurnsSincePraise == 0 {
		wins = append(wins, "the last turn earned positive feedback")
	}
	if baseline != nil && baseline.MedianFreq > 0 && len(recentTurns) > 0 {
		last := recentTurns[len(recentTurns)-1]
		if last.MedianFreq > 0 && last.MedianFreq <= baseline.MedianFreq+8 {
			wins = append(wins, "the latest turn stayed near the target depth")
		}
		if strings.Contains(last.PitchReport, "Voice dropped from") {
			wins = append(wins, "the voice settled downward during the turn")
		}
	}
	return clampStrings(wins, 2)
}

func findLastCorrectiveCue(recentMessages []llm.Message) string {
	for i := len(recentMessages) - 1; i >= 0; i-- {
		msg := recentMessages[i]
		if msg.Role != "assistant" || !isCorrectiveMessage(msg.Content) {
			continue
		}
		return compactSentence(msg.Content)
	}
	return ""
}

func inferWorkingCue(lastCue, previous string, signals coachingSignals) string {
	if lastCue == "" {
		return previous
	}
	if signals.PitchTrend == "trending higher" && signals.TurnsSincePraise > 0 {
		return ""
	}
	if signals.TurnsSincePraise == 0 || signals.PitchTrend == "trending lower" || signals.PitchTrend == "holding fairly steady" {
		return lastCue
	}
	return previous
}

func inferPracticeContext(recentTurns []TurnRecord) string {
	for i := len(recentTurns) - 1; i >= 0; i-- {
		text := strings.ToLower(strings.TrimSpace(recentTurns[i].UserText))
		if text == "" {
			continue
		}
		switch {
		case strings.Contains(text, "interview"):
			return "job interview practice"
		case strings.Contains(text, "meeting") || strings.Contains(text, "manager") || strings.Contains(text, "coworker") || strings.Contains(text, "client"):
			return "work conversation practice"
		case strings.Contains(text, "presentation") || strings.Contains(text, "speech") || strings.Contains(text, "talk"):
			return "presentation practice"
		case strings.Contains(text, "phone") || strings.Contains(text, "call") || strings.Contains(text, "voicemail"):
			return "phone call practice"
		case strings.Contains(text, "date") || strings.Contains(text, "dating"):
			return "date conversation practice"
		case strings.Contains(text, "story") || strings.Contains(text, "storytelling"):
			return "storytelling practice"
		case strings.Contains(text, "podcast") || strings.Contains(text, "video") || strings.Contains(text, "stream"):
			return "recorded speaking practice"
		case strings.Contains(text, "class") || strings.Contains(text, "lecture") || strings.Contains(text, "teach"):
			return "teaching practice"
		case strings.Contains(text, "sales") || strings.Contains(text, "pitching"):
			return "sales conversation practice"
		}
	}
	return "general conversation practice"
}

func countTurnsSinceAssistantFeedback(recentMessages []llm.Message, classifier func(string) bool) int {
	turns := 0
	for i := len(recentMessages) - 1; i >= 0; i-- {
		msg := recentMessages[i]
		if msg.Role != "assistant" {
			continue
		}
		if classifier(msg.Content) {
			return turns
		}
		turns++
	}
	return turns
}

func isPraiseMessage(text string) bool {
	normalized := " " + strings.ToLower(strings.TrimSpace(text)) + " "
	for _, phrase := range []string{
		" good ", " great ", " nice ", " strong ", " solid ", " better ", " steadier ",
		" deeper ", " easier ", " relaxed ", " smoother ", " that worked ", " keep that ",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func isCorrectiveMessage(text string) bool {
	normalized := " " + strings.ToLower(strings.TrimSpace(text)) + " "
	for _, phrase := range []string{
		" try ", " slow ", " slow down ", " relax ", " release ", " breathe ",
		" let it ", " drop ", " stay with ", " keep it ", " keep your ", " next one ",
		" speak from ", " chest ", " throat ", " ease up ", " back off ",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func isSelfMonitoringMessage(text string) bool {
	normalized := " " + strings.ToLower(strings.TrimSpace(text)) + " "
	for _, phrase := range []string{
		" did that feel ", " did that one feel ", " notice how ", " can you feel ",
		" does that feel ", " did you feel ", " hear how ", " felt easier ",
		" feel how ", " notice that ",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func isCarryoverMessage(text string) bool {
	normalized := " " + strings.ToLower(strings.TrimSpace(text)) + " "
	for _, phrase := range []string{
		" today ", " tonight ", " later today ", " next call ", " next meeting ",
		" one phone call ", " one conversation ", " one paragraph ", " when you ",
		" on your next ", " before we ", " after this session ", " practice this ",
		" try this with ", " use this voice when ",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func normalizeSessionMemory(memory SessionMemory) SessionMemory {
	memory.UserGoal = strings.TrimSpace(memory.UserGoal)
	memory.PracticeContext = strings.TrimSpace(memory.PracticeContext)
	memory.LastCue = compactSentence(memory.LastCue)
	memory.WorkingCue = compactSentence(memory.WorkingCue)
	memory.CarryoverFocus = strings.TrimSpace(memory.CarryoverFocus)
	memory.RecurringIssues = clampStrings(memory.RecurringIssues, 2)
	memory.RecentWins = clampStrings(memory.RecentWins, 2)
	return memory
}

func sessionMemoryEqual(a, b SessionMemory) bool {
	return a.UserGoal == b.UserGoal &&
		a.PracticeContext == b.PracticeContext &&
		a.LastCue == b.LastCue &&
		a.WorkingCue == b.WorkingCue &&
		a.CarryoverFocus == b.CarryoverFocus &&
		strings.Join(a.RecurringIssues, "|") == strings.Join(b.RecurringIssues, "|") &&
		strings.Join(a.RecentWins, "|") == strings.Join(b.RecentWins, "|")
}

func clampStrings(values []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, minInt(len(values), limit))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
		if len(out) == limit {
			break
		}
	}
	return out
}

func compactSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if idx := strings.IndexAny(text, ".!?"); idx >= 0 {
		text = text[:idx+1]
	}
	if len(text) > 120 {
		text = strings.TrimSpace(text[:120]) + "..."
	}
	return text
}

func joinQuoted(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func fallbackString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func buildCarryoverSuggestion(practiceContext string) string {
	switch practiceContext {
	case "job interview practice":
		return "answer one practice interview question out loud later today and keep the same slower, steadier delivery"
	case "work conversation practice":
		return "use this same relaxed, grounded voice in one work conversation or meeting today"
	case "presentation practice":
		return "read one short paragraph of your presentation out loud later and keep the same pace and resonance"
	case "phone call practice":
		return "use this same steady voice on your next phone call and keep the first two sentences relaxed"
	case "date conversation practice":
		return "tell one short story in a casual conversation later today and keep the same easy resonance"
	case "storytelling practice":
		return "retell one short story later today and keep the same pace and chest resonance"
	case "recorded speaking practice":
		return "record one brief take later today and keep the same relaxed onset and steadiness"
	case "teaching practice":
		return "explain one idea out loud later today and keep the same steady pace and support"
	case "sales conversation practice":
		return "deliver one short pitch later today and keep the same calm, grounded tone"
	default:
		return "use this same easier, steadier voice in one real conversation later today"
	}
}

func describeTrend(earlyAvg, recentAvg float64) string {
	delta := recentAvg - earlyAvg
	switch {
	case delta <= -10:
		return fmt.Sprintf("improving; recent turns are %.0f Hz lower than the opening turns", -delta)
	case delta >= 10:
		return fmt.Sprintf("slipping upward; recent turns are %.0f Hz higher than the opening turns", delta)
	default:
		return "holding fairly steady"
	}
}

func humanizeSince(startedAt time.Time) string {
	if startedAt.IsZero() {
		return "just now"
	}
	d := time.Since(startedAt).Round(time.Minute)
	if d < time.Minute {
		return "less than a minute"
	}
	return d.String()
}
