package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/etimbukafia/real-time-voice-pipeline-go/llm"
)

type CoachTools struct {
	Session *Session
}

func NewCoachTools(session *Session) *CoachTools {
	return &CoachTools{Session: session}
}

func (c *CoachTools) Definitions() []llm.Tool {
	return []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_pitch_history",
				Description: "Return recent per-turn pitch results for this session.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"limit": {
							"type": "integer",
							"description": "How many recent turns to return.",
							"minimum": 1,
							"maximum": 10
						}
					}
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_session_stats",
				Description: "Return overall pitch progress for the current session.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_baseline",
				Description: "Return the calibration baseline captured at the start of the session.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "compare_sessions",
				Description: "Compare the current session with the most recent prior session.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
	}
}

func (c *CoachTools) Execute(call llm.ToolCall) (string, error) {
	switch call.Function.Name {
	case "get_pitch_history":
		return c.getPitchHistory(call.Function.Arguments)
	case "get_session_stats":
		return c.getSessionStats()
	case "get_baseline":
		return c.getBaseline()
	case "compare_sessions":
		return c.compareSessions()
	default:
		return marshalToolResult(map[string]any{
			"error": fmt.Sprintf("unknown tool %q", call.Function.Name),
		})
	}
}

func (c *CoachTools) getPitchHistory(args string) (string, error) {
	var input struct {
		Limit int `json:"limit"`
	}
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			return marshalToolResult(map[string]any{
				"error": fmt.Sprintf("invalid arguments: %v", err),
			})
		}
	}
	if input.Limit <= 0 {
		input.Limit = 5
	}

	turns, err := c.Session.Store.RecentTurns(c.Session.ID, input.Limit)
	if err != nil {
		return marshalToolResult(map[string]any{"error": err.Error()})
	}

	type item struct {
		Turn     int     `json:"turn"`
		MedianHz float64 `json:"median_hz,omitempty"`
		Note     string  `json:"note,omitempty"`
		Type     string  `json:"type,omitempty"`
		LowNote  string  `json:"low_note,omitempty"`
		HighNote string  `json:"high_note,omitempty"`
	}
	out := make([]item, 0, len(turns))
	for _, turn := range turns {
		out = append(out, item{
			Turn:     turn.TurnIndex,
			MedianHz: turn.MedianFreq,
			Note:     turn.MedianNote,
			Type:     turn.VoiceType,
			LowNote:  turn.LowNote,
			HighNote: turn.HighNote,
		})
	}
	return marshalToolResult(out)
}

func (c *CoachTools) getSessionStats() (string, error) {
	stats, err := c.Session.Store.SessionStats(c.Session.ID)
	if err != nil {
		return marshalToolResult(map[string]any{"error": err.Error()})
	}

	result := map[string]any{
		"turns":            stats.Turns,
		"average_pitch_hz": stats.AvgPitch,
		"trend":            describeTrend(stats.EarlyAvg, stats.RecentAvg),
	}
	if stats.EarlyAvg > 0 {
		result["early_average_pitch_hz"] = stats.EarlyAvg
	}
	if stats.RecentAvg > 0 {
		result["recent_average_pitch_hz"] = stats.RecentAvg
	}
	if stats.HasBestTurn {
		result["best_turn"] = stats.BestTurn.TurnIndex
		result["best_turn_pitch_hz"] = stats.BestTurn.MedianFreq
		result["best_turn_note"] = stats.BestTurn.MedianNote
	}
	return marshalToolResult(result)
}

func (c *CoachTools) getBaseline() (string, error) {
	c.Session.mu.Lock()
	baseline := c.Session.Baseline
	c.Session.mu.Unlock()
	if baseline == nil || baseline.Name == "" {
		return marshalToolResult(map[string]any{"error": "baseline not available"})
	}

	return marshalToolResult(map[string]any{
		"freq_hz":   baseline.MedianFreq,
		"note":      baseline.MedianNote,
		"type":      baseline.Name,
		"low_note":  baseline.LowNote,
		"high_note": baseline.HighNote,
	})
}

func (c *CoachTools) compareSessions() (string, error) {
	stats, err := c.Session.Store.SessionStats(c.Session.ID)
	if err != nil {
		return marshalToolResult(map[string]any{"error": err.Error()})
	}
	previous, err := c.Session.Store.PreviousSessions(c.Session.ID, 1)
	if err != nil {
		return marshalToolResult(map[string]any{"error": err.Error()})
	}
	if len(previous) == 0 || previous[0].AvgPitch <= 0 || stats.AvgPitch <= 0 {
		return marshalToolResult(map[string]any{"error": "not enough prior session data"})
	}

	prev := previous[0]
	improvement := 0.0
	if prev.AvgPitch > 0 {
		improvement = ((prev.AvgPitch - stats.AvgPitch) / prev.AvgPitch) * 100
	}

	return marshalToolResult(map[string]any{
		"current_average_pitch_hz":  stats.AvgPitch,
		"previous_session_id":       prev.ID,
		"previous_average_pitch_hz": prev.AvgPitch,
		"delta_hz":                  prev.AvgPitch - stats.AvgPitch,
		"improvement_percent":       improvement,
	})
}

func marshalToolResult(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal tool result: %w", err)
	}
	return string(data), nil
}
