package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/etimbukafia/speech-coach/pitch"
)

type Store struct {
	db *sql.DB
}

type TurnRecord struct {
	TurnIndex   int       `json:"turn"`
	Timestamp   time.Time `json:"timestamp"`
	UserText    string    `json:"user_text,omitempty"`
	CoachText   string    `json:"coach_text,omitempty"`
	MedianFreq  float64   `json:"median_freq,omitempty"`
	MedianNote  string    `json:"median_note,omitempty"`
	VoiceType   string    `json:"voice_type,omitempty"`
	LowNote     string    `json:"low_note,omitempty"`
	HighNote    string    `json:"high_note,omitempty"`
	PitchReport string    `json:"pitch_report,omitempty"`
}

type SessionStats struct {
	Turns       int
	AvgPitch    float64
	EarlyAvg    float64
	RecentAvg   float64
	BestTurn    TurnRecord
	HasBestTurn bool
}

type SessionSummary struct {
	ID           string
	StartedAt    time.Time
	BaselineFreq float64
	BaselineNote string
	BaselineType string
	BaselineLow  string
	BaselineHigh string
	MemoryJSON   string
	Turns        int
	AvgPitch     float64
}

func OpenStore(path string) (*Store, error) {
	resolved, err := resolveDBPath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return nil, fmt.Errorf("store: create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", resolved)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}

	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    started_at DATETIME NOT NULL,
    baseline_freq REAL,
    baseline_note TEXT,
    baseline_type TEXT,
    baseline_low_note TEXT,
    baseline_high_note TEXT,
    session_memory TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS turns (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    turn_index INTEGER NOT NULL,
    timestamp DATETIME NOT NULL,
    user_text TEXT,
    coach_text TEXT,
    median_freq REAL,
    median_note TEXT,
    voice_type TEXT,
    low_note TEXT,
    high_note TEXT,
    pitch_report TEXT,
    UNIQUE(session_id, turn_index)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: init schema: %w", err)
	}
	if err := s.ensureColumn("sessions", "session_memory", "ALTER TABLE sessions ADD COLUMN session_memory TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateSession(id string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO sessions (id, started_at) VALUES (?, ?)`,
		id,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

func (s *Store) LoadSession(sessionID string) (SessionSummary, error) {
	row := s.db.QueryRow(
		`SELECT
			s.id,
			s.started_at,
			COALESCE(s.baseline_freq, 0),
			COALESCE(s.baseline_note, ''),
			COALESCE(s.baseline_type, ''),
			COALESCE(s.baseline_low_note, ''),
			COALESCE(s.baseline_high_note, ''),
			COALESCE(s.session_memory, ''),
			COUNT(t.id),
			COALESCE(AVG(NULLIF(t.median_freq, 0)), 0)
		 FROM sessions s
		 LEFT JOIN turns t ON t.session_id = s.id
		 WHERE s.id = ?
		 GROUP BY s.id, s.started_at, s.baseline_freq, s.baseline_note, s.baseline_type, s.baseline_low_note, s.baseline_high_note, s.session_memory`,
		sessionID,
	)

	var summary SessionSummary
	if err := row.Scan(
		&summary.ID,
		&summary.StartedAt,
		&summary.BaselineFreq,
		&summary.BaselineNote,
		&summary.BaselineType,
		&summary.BaselineLow,
		&summary.BaselineHigh,
		&summary.MemoryJSON,
		&summary.Turns,
		&summary.AvgPitch,
	); err != nil {
		if err == sql.ErrNoRows {
			return SessionSummary{}, fmt.Errorf("store: load session %s: %w", sessionID, sql.ErrNoRows)
		}
		return SessionSummary{}, fmt.Errorf("store: load session %s: %w", sessionID, err)
	}
	return summary, nil
}

func (s *Store) SaveBaseline(sessionID string, v pitch.VoiceType) error {
	_, err := s.db.Exec(
		`UPDATE sessions
		 SET baseline_freq = ?, baseline_note = ?, baseline_type = ?, baseline_low_note = ?, baseline_high_note = ?
		 WHERE id = ?`,
		v.MedianFreq,
		v.MedianNote,
		v.Name,
		v.LowNote,
		v.HighNote,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: save baseline: %w", err)
	}
	return nil
}

func (s *Store) SaveSessionMemory(sessionID string, memory SessionMemory) error {
	payload, err := json.Marshal(memory)
	if err != nil {
		return fmt.Errorf("store: encode session memory: %w", err)
	}
	_, err = s.db.Exec(
		`UPDATE sessions
		 SET session_memory = ?
		 WHERE id = ?`,
		string(payload),
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: save session memory: %w", err)
	}
	return nil
}

func (s *Store) SaveTurn(sessionID string, t TurnRecord) error {
	ts := t.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO turns (
			session_id, turn_index, timestamp, user_text, coach_text,
			median_freq, median_note, voice_type, low_note, high_note, pitch_report
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, turn_index) DO UPDATE SET
			timestamp = excluded.timestamp,
			user_text = excluded.user_text,
			coach_text = CASE
				WHEN excluded.coach_text <> '' THEN excluded.coach_text
				ELSE turns.coach_text
			END,
			median_freq = excluded.median_freq,
			median_note = excluded.median_note,
			voice_type = excluded.voice_type,
			low_note = excluded.low_note,
			high_note = excluded.high_note,
			pitch_report = excluded.pitch_report`,
		sessionID,
		t.TurnIndex,
		ts,
		t.UserText,
		t.CoachText,
		t.MedianFreq,
		t.MedianNote,
		t.VoiceType,
		t.LowNote,
		t.HighNote,
		t.PitchReport,
	)
	if err != nil {
		return fmt.Errorf("store: save turn %d: %w", t.TurnIndex, err)
	}
	return nil
}

func (s *Store) RecentTurns(sessionID string, limit int) ([]TurnRecord, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.Query(
		`SELECT turn_index, timestamp, user_text, coach_text, median_freq, median_note, voice_type, low_note, high_note, pitch_report
		 FROM turns
		 WHERE session_id = ?
		 ORDER BY turn_index DESC
		 LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: recent turns: %w", err)
	}
	defer rows.Close()

	var turns []TurnRecord
	for rows.Next() {
		var rec TurnRecord
		if err := rows.Scan(
			&rec.TurnIndex,
			&rec.Timestamp,
			&rec.UserText,
			&rec.CoachText,
			&rec.MedianFreq,
			&rec.MedianNote,
			&rec.VoiceType,
			&rec.LowNote,
			&rec.HighNote,
			&rec.PitchReport,
		); err != nil {
			return nil, fmt.Errorf("store: scan recent turn: %w", err)
		}
		turns = append(turns, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent turns rows: %w", err)
	}

	reverseTurns(turns)
	return turns, nil
}

func (s *Store) SessionStats(sessionID string) (SessionStats, error) {
	rows, err := s.db.Query(
		`SELECT turn_index, timestamp, user_text, coach_text, median_freq, median_note, voice_type, low_note, high_note, pitch_report
		 FROM turns
		 WHERE session_id = ?
		 ORDER BY turn_index ASC`,
		sessionID,
	)
	if err != nil {
		return SessionStats{}, fmt.Errorf("store: session stats query: %w", err)
	}
	defer rows.Close()

	var (
		stats SessionStats
		all   []TurnRecord
	)
	for rows.Next() {
		var rec TurnRecord
		if err := rows.Scan(
			&rec.TurnIndex,
			&rec.Timestamp,
			&rec.UserText,
			&rec.CoachText,
			&rec.MedianFreq,
			&rec.MedianNote,
			&rec.VoiceType,
			&rec.LowNote,
			&rec.HighNote,
			&rec.PitchReport,
		); err != nil {
			return SessionStats{}, fmt.Errorf("store: session stats scan: %w", err)
		}
		all = append(all, rec)
	}
	if err := rows.Err(); err != nil {
		return SessionStats{}, fmt.Errorf("store: session stats rows: %w", err)
	}

	stats.Turns = len(all)
	var pitchTurns []TurnRecord
	for _, rec := range all {
		if rec.MedianFreq <= 0 {
			continue
		}
		pitchTurns = append(pitchTurns, rec)
		stats.AvgPitch += rec.MedianFreq
		if !stats.HasBestTurn || rec.MedianFreq < stats.BestTurn.MedianFreq {
			stats.BestTurn = rec
			stats.HasBestTurn = true
		}
	}
	if len(pitchTurns) > 0 {
		stats.AvgPitch /= float64(len(pitchTurns))
		stats.EarlyAvg = averagePitch(pitchTurns[:minInt(3, len(pitchTurns))])
		stats.RecentAvg = averagePitch(pitchTurns[maxInt(0, len(pitchTurns)-3):])
	}

	return stats, nil
}

func (s *Store) PreviousSessions(currentSessionID string, limit int) ([]SessionSummary, error) {
	if limit <= 0 {
		limit = 3
	}
	rows, err := s.db.Query(
		`SELECT
			s.id,
			s.started_at,
			COALESCE(s.baseline_freq, 0),
			COALESCE(s.baseline_note, ''),
			COALESCE(s.baseline_type, ''),
			COALESCE(s.baseline_low_note, ''),
			COALESCE(s.baseline_high_note, ''),
			COUNT(t.id),
			COALESCE(AVG(NULLIF(t.median_freq, 0)), 0)
		 FROM sessions s
		 LEFT JOIN turns t ON t.session_id = s.id
		 WHERE s.id <> ?
		 GROUP BY s.id, s.started_at, s.baseline_freq, s.baseline_note, s.baseline_type, s.baseline_low_note, s.baseline_high_note
		 ORDER BY s.started_at DESC
		 LIMIT ?`,
		currentSessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: previous sessions query: %w", err)
	}
	defer rows.Close()

	var sessions []SessionSummary
	for rows.Next() {
		var summary SessionSummary
		if err := rows.Scan(
			&summary.ID,
			&summary.StartedAt,
			&summary.BaselineFreq,
			&summary.BaselineNote,
			&summary.BaselineType,
			&summary.BaselineLow,
			&summary.BaselineHigh,
			&summary.Turns,
			&summary.AvgPitch,
		); err != nil {
			return nil, fmt.Errorf("store: previous sessions scan: %w", err)
		}
		sessions = append(sessions, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: previous sessions rows: %w", err)
	}
	return sessions, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func BuildTurnRecord(turnIndex int, userText string, pitches []pitch.Result) TurnRecord {
	rec := TurnRecord{
		TurnIndex:   turnIndex,
		Timestamp:   time.Now().UTC(),
		UserText:    userText,
		PitchReport: pitch.PitchReport(pitches),
	}
	if len(pitches) == 0 {
		return rec
	}
	voice := pitch.ClassifyVoice(pitches)
	rec.MedianFreq = voice.MedianFreq
	rec.MedianNote = voice.MedianNote
	rec.VoiceType = voice.Name
	rec.LowNote = voice.LowNote
	rec.HighNote = voice.HighNote
	return rec
}

func resolveDBPath(path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: determine home dir: %w", err)
	}
	return filepath.Join(home, ".voice-coach", "coach.db"), nil
}

func averagePitch(turns []TurnRecord) float64 {
	if len(turns) == 0 {
		return 0
	}
	var total float64
	for _, turn := range turns {
		total += turn.MedianFreq
	}
	return total / float64(len(turns))
}

func reverseTurns(turns []TurnRecord) {
	for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
		turns[left], turns[right] = turns[right], turns[left]
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("store: inspect %s schema: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			typeName   string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultVal, &primaryKey); err != nil {
			return fmt.Errorf("store: scan %s schema: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: inspect %s schema rows: %w", table, err)
	}
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("store: add %s.%s column: %w", table, column, err)
	}
	return nil
}

func decodeSessionMemory(raw string) (SessionMemory, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return SessionMemory{}, nil
	}
	var memory SessionMemory
	if err := json.Unmarshal([]byte(raw), &memory); err != nil {
		return SessionMemory{}, fmt.Errorf("decode session memory: %w", err)
	}
	return normalizeSessionMemory(memory), nil
}
