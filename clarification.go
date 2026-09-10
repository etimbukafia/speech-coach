package main

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/etimbukafia/speech-coach/pitch"
)

type pendingClarification struct {
	TurnIndex          int
	Transcript         string
	Pitches            []pitch.Result
	Confidence         float64
	AwaitingCorrection bool
}

func shouldClarifyTranscript(text string, confidence float64) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}

	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 || len(words) > 4 {
		return false
	}

	if confidence > 0 && confidence < 0.78 {
		return true
	}

	if len(words) != 1 {
		return false
	}

	token := sanitizeClarificationToken(words[0])
	if len(token) >= 10 {
		return true
	}
	return hasSuspiciousTokenShape(token)
}

func sanitizeClarificationToken(token string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r):
			return unicode.ToLower(r)
		case r == '\'' || r == '-':
			return r
		default:
			return -1
		}
	}, token)
}

func hasSuspiciousTokenShape(token string) bool {
	if len(token) < 8 {
		return false
	}

	consonantRun := 0
	var prev rune
	hasPrev := false
	for _, r := range token {
		if !unicode.IsLetter(r) {
			consonantRun = 0
			hasPrev = false
			continue
		}
		if isVowel(r) {
			consonantRun = 0
		} else {
			consonantRun++
			if consonantRun >= 4 {
				return true
			}
			if hasPrev && prev == r && consonantRun >= 3 {
				return true
			}
		}
		prev = r
		hasPrev = true
	}

	return false
}

func isVowel(r rune) bool {
	switch unicode.ToLower(r) {
	case 'a', 'e', 'i', 'o', 'u', 'y':
		return true
	default:
		return false
	}
}

func clarificationPrompt(text string) string {
	return fmt.Sprintf("I heard \"%s.\" Is that right? Say yes, or say it again.", strings.TrimSpace(text))
}

func repeatPrompt() string {
	return "Okay. Say it once more, and I'll use that version."
}

func isAffirmativeClarification(text string) bool {
	switch normalizeClarificationReply(text) {
	case "yes", "yeah", "yep", "correct", "that's right", "that is right", "right", "exactly", "mm-hmm", "mhm":
		return true
	default:
		return false
	}
}

func isNegativeClarification(text string) bool {
	switch normalizeClarificationReply(text) {
	case "no", "nope", "nah", "not quite", "wrong", "incorrect":
		return true
	default:
		return false
	}
}

func normalizeClarificationReply(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.Trim(text, " .,!?:;\"'")
	return strings.Join(strings.Fields(text), " ")
}
