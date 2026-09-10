package main

import "testing"

func TestShouldClarifyTranscriptLowConfidenceShortUtterance(t *testing.T) {
	if !shouldClarifyTranscript("what drop", 0.52) {
		t.Fatal("expected low-confidence short utterance to trigger clarification")
	}
}

func TestShouldClarifyTranscriptSuspiciousSingleToken(t *testing.T) {
	if !shouldClarifyTranscript("warddrope", 0.91) {
		t.Fatal("expected suspicious token shape to trigger clarification")
	}
}

func TestShouldNotClarifyNormalUtterance(t *testing.T) {
	if shouldClarifyTranscript("what's your name", 0.93) {
		t.Fatal("did not expect normal high-confidence utterance to trigger clarification")
	}
}

func TestClarificationReplyDetection(t *testing.T) {
	if !isAffirmativeClarification("yeah") {
		t.Fatal("expected affirmative clarification reply")
	}
	if !isNegativeClarification("nope") {
		t.Fatal("expected negative clarification reply")
	}
	if isAffirmativeClarification("what drop") {
		t.Fatal("did not expect corrected phrase to count as affirmative")
	}
}
