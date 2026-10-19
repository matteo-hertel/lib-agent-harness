package nativecli

import (
	"encoding/json"
	"strings"
)

// AntigravityError is agy's error field. agy 1.4.2 sends a plain string on a
// terminal result and an object with a message on a tool step.
type AntigravityError struct {
	Message string
}

func (e *AntigravityError) UnmarshalJSON(raw []byte) error {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		e.Message = text
		return nil
	}
	var object struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	e.Message = object.Message
	return nil
}

// AntigravityAuthPrompt reports agy's logged-out stderr prompt. A logged-out
// agy prints it at once, then waits a minute for a browser login before its
// result says "authentication failed or timed out".
func AntigravityAuthPrompt(line string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "authentication required")
}

// AntigravityAuthFailure reports a terminal result's logged-out error.
func AntigravityAuthFailure(message string) bool {
	return strings.HasPrefix(strings.ToLower(message), "authentication failed")
}
