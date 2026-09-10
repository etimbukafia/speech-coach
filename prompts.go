package main

import (
	_ "embed"
	"strings"
)

var (
	//go:embed prompts/coach_system.md
	coachSystemPromptSource string

	//go:embed prompts/calibration.md
	calibrationPromptSource string

	coachSystemPrompt = strings.TrimSpace(coachSystemPromptSource)
	calibrationPrompt = strings.TrimSpace(calibrationPromptSource)
)
