package session

import (
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// Antigravity's stream-json turn events (`step_update`, `error`, `result`).

const (
	antigravityTurnFailed         = "turn_failed"
	antigravityCanceled           = "canceled"
	antigravityInterrupted        = "interrupted"
	antigravityInvalid            = "invalid"
	antigravityStatusUnrecognized = "status_unrecognized"
)

var antigravityToolIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)

func (s *Session) antigravityEvent(t *Turn, ref Ref, m map[string]json.RawMessage) {
	if id := str(m, "conversation_id"); id != "" && ref.ID != "" && id != ref.ID {
		s.fail(ErrProtocol)
		return
	}
	switch str(m, "event") {
	case "step_update":
		s.antigravityStepUpdate(t, m["step_update"])
	case "error":
		if report := s.options.OnDiagnostic; report != nil {
			report(Diagnostic{Engine: harness.Antigravity, Stage: "turn_error", Code: "error_event", Detail: sanitize(m["error"], 1024), At: time.Now().UTC()})
		}
	case "result":
		s.antigravityResult(t, m["result"])
	}
}

func (s *Session) antigravityStepUpdate(t *Turn, raw json.RawMessage) {
	var u struct {
		StepIndex *int            `json:"step_index"`
		StepType  string          `json:"step_type"`
		State     string          `json:"state"`
		TextDelta string          `json:"text_delta"`
		ToolName  string          `json:"tool_name"`
		Usage     json.RawMessage `json:"usage"`
		ToolInfo  *struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
			Output     string          `json:"output"`
			Error      *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"tool_info"`
	}
	if json.Unmarshal(raw, &u) != nil || u.StepIndex == nil || *u.StepIndex < 0 {
		return
	}
	itemID := "step-" + strconv.Itoa(*u.StepIndex)
	switch u.StepType {
	case "agent_response":
		if u.TextDelta != "" {
			s.text(t, itemID, u.TextDelta, false)
		}
		if u.State == "DONE" && !rawjson.Absent(u.Usage) {
			if usage := parseAntigravityUsage(u.Usage); usage.Known {
				s.observeRequestUsage(t, usage)
			}
		}
	case "tool":
		name := u.ToolName
		var input json.RawMessage
		var output string
		if u.ToolInfo != nil {
			if name == "" {
				name = u.ToolInfo.Name
			}
			if !rawjson.Absent(u.ToolInfo.Parameters) {
				input = u.ToolInfo.Parameters
			}
			output = u.ToolInfo.Output
			if output == "" && u.ToolInfo.Error != nil {
				output = u.ToolInfo.Error.Message
			}
		}
		if !antigravityToolIdentifier.MatchString(name) {
			name = "tool"
		}
		t.mu.Lock()
		if t.antigravityTools == nil {
			t.antigravityTools = map[string]bool{}
		}
		started := t.antigravityTools[itemID]
		if !started {
			t.antigravityTools[itemID] = true
		}
		t.mu.Unlock()
		if !started {
			s.emit(t, s.withToolPayload(Event{Kind: "tool_started", ItemID: itemID, Tool: name, Status: "in_progress"}, input, ""))
		}
		switch u.State {
		case "DONE", "ERROR", "CANCELED", "INTERRUPTED":
			doneKey := itemID + ":done"
			t.mu.Lock()
			alreadyDone := t.antigravityTools[doneKey]
			if !alreadyDone {
				t.antigravityTools[doneKey] = true
			}
			t.mu.Unlock()
			if !alreadyDone {
				status := "completed"
				if u.State != "DONE" {
					status = "failed"
				}
				s.emit(t, s.withToolPayload(Event{Kind: "tool_completed", ItemID: itemID, Tool: name, Status: status}, input, output))
			}
		}
	}
}

func (s *Session) antigravityResult(t *Turn, raw json.RawMessage) {
	var r struct {
		Status   string          `json:"status"`
		Response string          `json:"response"`
		Error    json.RawMessage `json:"error"`
		Usage    json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Status == "" {
		s.fail(ErrProtocol)
		return
	}
	status, code := antigravityOutcome(r.Status, !rawjson.Absent(r.Error))
	if status == "completed" && r.Response != "" {
		s.text(t, "result", r.Response, true)
	}
	var usage Usage
	if status == "completed" && !rawjson.Absent(r.Usage) {
		usage = parseAntigravityUsage(r.Usage)
	}
	var err error
	t.mu.Lock()
	if usage.Known {
		usage.Final = true
		t.result.Usage = usage
	}
	if code != "" {
		t.result.NativeError = true
		err = &TurnError{Engine: harness.Antigravity, Code: code}
	}
	t.mu.Unlock()
	if code != "" && !rawjson.Absent(r.Error) {
		if report := s.options.OnDiagnostic; report != nil {
			report(Diagnostic{Engine: harness.Antigravity, Stage: "turn_result", Code: code, Detail: sanitize(r.Error, 1024), At: time.Now().UTC()})
		}
	}
	if usage.Known {
		s.emit(t, Event{Kind: "usage", Usage: &usage})
	}
	s.emit(t, Event{Kind: "status", Status: status})
	t.finish(status, err)
}

func antigravityOutcome(status string, hasError bool) (string, string) {
	switch status {
	case "SUCCESS":
		if hasError {
			return "failed", antigravityTurnFailed
		}
		return "completed", ""
	case "CANCELED":
		return "failed", antigravityCanceled
	case "INTERRUPTED":
		return "failed", antigravityInterrupted
	case "ERROR":
		return "failed", antigravityTurnFailed
	case "INVALID":
		return "failed", antigravityInvalid
	}
	return "failed", antigravityStatusUnrecognized
}

func parseAntigravityUsage(raw json.RawMessage) Usage {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return Usage{}
	}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if rawjson.Absent(fields[key]) {
			return Usage{}
		}
	}
	var c struct {
		Input     *int64 `json:"input_tokens"`
		Output    *int64 `json:"output_tokens"`
		Reasoning *int64 `json:"thinking_tokens"`
		Read      *int64 `json:"cache_read_tokens"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Input == nil || c.Output == nil {
		return Usage{}
	}
	read, reasoning := valueOr(c.Read), valueOr(c.Reasoning)
	input, output := *c.Input, *c.Output
	if input < 0 || output < 0 || read < 0 || reasoning < 0 || input > maxInt64-read || output > maxInt64-reasoning {
		return Usage{}
	}
	return Usage{Usage: harness.Usage{
		Known:          true,
		Input:          input + read,
		Output:         output + reasoning,
		CacheRead:      read,
		Reasoning:      reasoning,
		CacheKnown:     c.Read != nil,
		ReasoningKnown: c.Reasoning != nil,
	}}
}
