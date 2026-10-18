package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// A synthetic Antigravity CLI: this test binary re-executed as
// `agy -p= --input-format=stream-json --output-format=stream-json ...`.
// It answers with the shapes agy 0.2.2 was observed to use, performs no
// inference and reads no login.
const (
	antigravityFixtureEnv   = "LIB_HARNESS_AGY_FIXTURE"
	antigravityFixtureLog   = "LIB_HARNESS_AGY_FIXTURE_LOG"
	antigravityFixtureModel = "LIB_HARNESS_AGY_FIXTURE_MODEL"
)

func init() {
	if os.Getenv(antigravityFixtureEnv) != "1" {
		return
	}
	os.Exit(runAntigravityFixture())
}

func runAntigravityFixture() int {
	logFile, err := os.OpenFile(os.Getenv(antigravityFixtureLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 2
	}
	defer logFile.Close()
	record := func(kind, value string) { _, _ = logFile.WriteString(kind + "\t" + value + "\n") }
	record("args", strings.Join(os.Args[1:], " "))
	record("gemini", os.Getenv("GEMINI_API_KEY"))
	record("agy", os.Getenv("AGY_TOKEN"))
	record("autoupdate", os.Getenv("AGY_CLI_DISABLE_AUTO_UPDATE"))

	model, mode, convID := "gemini-3-flash", "request-review", "agy-conv-1"
	for _, arg := range os.Args[1:] {
		if value, ok := strings.CutPrefix(arg, "--model="); ok {
			model = value
		}
		if value, ok := strings.CutPrefix(arg, "--mode="); ok {
			mode = value
		}
		if value, ok := strings.CutPrefix(arg, "--conversation="); ok {
			record("resume", value)
			if value == "agy-conv-1" {
				convID = value
			} else {
				// agy creates a fresh conversation when the requested ID does not exist.
				convID = "agy-conv-fresh"
			}
		}
	}
	if override := os.Getenv(antigravityFixtureModel); override != "" {
		model = override
	}

	out := json.NewEncoder(os.Stdout)
	send := func(m map[string]any) bool { return out.Encode(m) == nil }
	if !send(map[string]any{
		"type":            "event",
		"event":           "init",
		"timestamp":       1776492727535,
		"conversation_id": convID,
		"init": map[string]any{
			"cwd":             ".",
			"model":           model,
			"tools":           []string{"view_file", "run_command"},
			"permission_mode": mode,
		},
	}) {
		return 3
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for in.Scan() {
		var frame struct {
			Event   string `json:"event"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(in.Bytes(), &frame) != nil || frame.Event != "user" || frame.Message.Content == "" {
			return 3
		}
		text := frame.Message.Content
		record("prompt", text)
		switch {
		case strings.HasSuffix(text, "fail"):
			send(map[string]any{
				"type":            "event",
				"event":           "error",
				"conversation_id": convID,
				"error":           map[string]any{"message": "secret provider stream error"},
			})
			send(map[string]any{
				"type":            "event",
				"event":           "result",
				"conversation_id": convID,
				"result": map[string]any{
					"status": "ERROR",
					"error":  map[string]any{"message": "secret provider result error"},
				},
			})
		default:
			send(map[string]any{
				"type":            "event",
				"event":           "step_update",
				"conversation_id": convID,
				"step_update": map[string]any{
					"step_index": 1,
					"step_type":  "tool",
					"state":      "IN_PROGRESS",
					"tool_name":  "view_file",
					"tool_info": map[string]any{
						"name":       "view_file",
						"parameters": map[string]any{"AbsolutePath": "/workspace/README.md"},
					},
				},
			})
			send(map[string]any{
				"type":            "event",
				"event":           "step_update",
				"conversation_id": convID,
				"step_update": map[string]any{
					"step_index": 1,
					"step_type":  "tool",
					"state":      "DONE",
					"tool_name":  "view_file",
					"tool_info": map[string]any{
						"name":       "view_file",
						"parameters": map[string]any{"AbsolutePath": "/workspace/README.md"},
						"output":     "file contents",
					},
				},
			})
			send(map[string]any{
				"type":            "event",
				"event":           "step_update",
				"conversation_id": convID,
				"step_update": map[string]any{
					"step_index": 2,
					"step_type":  "agent_response",
					"state":      "GENERATING",
					"text_delta": "fixture ",
				},
			})
			send(map[string]any{
				"type":            "event",
				"event":           "step_update",
				"conversation_id": convID,
				"step_update": map[string]any{
					"step_index": 2,
					"step_type":  "agent_response",
					"state":      "DONE",
					"text_delta": "answer",
					"usage": map[string]any{
						"model":             model,
						"input_tokens":      10,
						"output_tokens":     4,
						"thinking_tokens":   2,
						"cache_read_tokens": 30,
					},
				},
			})
			send(map[string]any{
				"type":            "event",
				"event":           "result",
				"conversation_id": convID,
				"result": map[string]any{
					"status":      "SUCCESS",
					"response":    "fixture answer",
					"duration_ms": 120,
					"num_turns":   1,
					"usage": map[string]any{
						"model":             model,
						"input_tokens":      15,
						"output_tokens":     6,
						"thinking_tokens":   2,
						"cache_read_tokens": 70,
					},
				},
			})
		}
	}
	return 0
}

func antigravityFixtureOptions(t *testing.T) (Options, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "fixture.log")
	t.Setenv(antigravityFixtureEnv, "1")
	t.Setenv(antigravityFixtureLog, log)
	return Options{
		Provider: harness.Provider{Engine: harness.Antigravity, CLI: harness.CLI{Binary: os.Args[0]}},
		WorkDir:  t.TempDir(),
	}, log
}

func TestAntigravitySessionOverStreamJSON(t *testing.T) {
	o, log := antigravityFixtureOptions(t)
	t.Setenv("GEMINI_API_KEY", "ambient-secret")
	t.Setenv("AGY_TOKEN", "ambient-token")
	o.Model, o.Effort = "gemini-3.1-pro-high", "low"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Ref()
	if ref.Engine != harness.Antigravity || ref.ID != "agy-conv-1" {
		t.Fatalf("reference: %+v", ref)
	}
	caps := s.Capabilities()
	if caps.Start.Availability != harness.Native || caps.Resume.Availability != harness.Unknown || caps.Interrupt.Availability != harness.Unsupported || caps.Steer.Availability != harness.Unsupported {
		t.Fatalf("capabilities: %+v", caps)
	}

	turn, err := s.StartTurn(ctx, Input{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(t, turn)
	result, err := turn.Wait(ctx)
	if err != nil || result.Status != "completed" || result.Text != "fixture answer" {
		t.Fatalf("%+v %v", result, err)
	}

	var kinds []string
	var observed []Usage
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		if e.Kind == "tool_started" && (e.Tool != "view_file" || e.ItemID != "step-1" || !strings.Contains(string(e.Input), "README.md")) {
			t.Errorf("tool_started event: %+v", e)
		}
		if e.Kind == "tool_completed" && (e.Tool != "view_file" || e.ItemID != "step-1" || e.Output != "file contents" || e.Status != "completed") {
			t.Errorf("tool_completed event: %+v", e)
		}
		if e.Kind == "usage" {
			observed = append(observed, *e.Usage)
		}
	}
	if slices.Index(kinds, "tool_started") < 0 || slices.Index(kinds, "tool_completed") < 0 || kinds[len(kinds)-1] != "status" {
		t.Fatalf("events: %v", kinds)
	}
	if len(observed) != 2 || observed[0].Final || observed[0].Input != 40 || observed[0].Output != 6 || !observed[1].Final || observed[1].Input != 85 || observed[1].Output != 8 {
		t.Fatalf("usage observations: %+v", observed)
	}
	final := result.Usage
	if !final.Final || final.Input != 85 || final.CacheRead != 70 || final.Output != 8 || final.Reasoning != 2 || !final.CacheKnown || !final.ReasoningKnown {
		t.Fatalf("turn accounting: %+v", final)
	}
	if fresh, ok := final.Fresh(); !ok || fresh != 15 {
		t.Fatalf("fresh input %d %v", fresh, ok)
	}

	if err = s.Interrupt(ctx, turn.ID()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("interrupt: %v", err)
	}
	if _, err = s.Steer(ctx, turn.ID(), Input{Text: "next"}, SteerOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("steer: %v", err)
	}
	if _, err = s.Compact(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("compact: %v", err)
	}
	if _, err = s.ReadAccount(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("account: %v", err)
	}
	if _, err = s.ReadQuota(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("quota: %v", err)
	}
	if _, err = s.ReadContext(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("context: %v", err)
	}
	if _, err = Inspect(ctx, o); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("inspect: %v", err)
	}
	s.Close()

	logged := grokFixtureLogged(t, log)
	if logged["args"][0] != "-p= --input-format=stream-json --output-format=stream-json --mode=request-review --model=gemini-3.1-pro-high --effort=low" {
		t.Errorf("arguments: %q", logged["args"])
	}
	if logged["gemini"][0] != "" || logged["agy"][0] != "" || logged["autoupdate"][0] != "1" {
		t.Errorf("environment: gemini %q agy %q autoupdate %q", logged["gemini"], logged["agy"], logged["autoupdate"])
	}

	resumed, err := Resume(ctx, o, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.Ref() != ref || resumed.Capabilities().Resume.Availability != harness.Native {
		t.Fatal("resume changed the reference or failed to record capability")
	}
	logged = grokFixtureLogged(t, log)
	if len(logged["resume"]) != 1 || logged["resume"][0] != "agy-conv-1" {
		t.Fatalf("resume args: %v", logged["resume"])
	}
}

func TestAntigravityOpenStartsFreshWhenTheConversationIsGone(t *testing.T) {
	o, _ := antigravityFixtureOptions(t)
	ctx := testContext(t)
	n, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	gone := reference(n, "agy-conv-gone")
	if _, err := Resume(ctx, o, gone); !errors.Is(err, ErrRejected) {
		t.Fatalf("expected Resume to fail with ErrRejected, got %v", err)
	}
	s, opened, err := Open(ctx, o, &gone)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if opened.Fresh != FreshUnavailable || s.Ref().ID != "agy-conv-1" {
		t.Fatalf("%+v %+v", opened, s.Ref())
	}
}

func TestAntigravitySubstitutedModelIsRefused(t *testing.T) {
	o, _ := antigravityFixtureOptions(t)
	t.Setenv(antigravityFixtureModel, "gemini-3-flash")
	o.Model = "gemini-3.1-pro-high"
	_, err := Start(testContext(t), o)
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityChangedModel || failure.Phase != BeforeFirstPrompt {
		t.Fatalf("expected CapabilityChangedModel before first prompt, got %v", err)
	}
}

func TestAntigravityRefusedPromptFailsOnlyItsTurn(t *testing.T) {
	o, _ := antigravityFixtureOptions(t)
	var mu sync.Mutex
	var diagnostics []Diagnostic
	o.OnDiagnostic = func(d Diagnostic) {
		mu.Lock()
		diagnostics = append(diagnostics, d)
		mu.Unlock()
	}
	ctx := testContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	turn, err := s.StartTurn(ctx, Input{Text: "fail"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	result, err := turn.Wait(ctx)
	var failure *TurnError
	if !errors.As(err, &failure) || failure.Code != antigravityTurnFailed || result.Usage.Known || !result.NativeError {
		t.Fatalf("%+v %v", result, err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("provider text reached the error")
	}
	mu.Lock()
	diagCount := len(diagnostics)
	mu.Unlock()
	if diagCount < 2 {
		t.Fatalf("expected diagnostics for error event and result error, got %d", diagCount)
	}

	turn, err = s.StartTurn(ctx, Input{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	if result, err = turn.Wait(ctx); err != nil || result.Status != "completed" {
		t.Fatalf("the session did not survive a failed turn: %+v %v", result, err)
	}
}

func TestAntigravityOutcomesAndUsage(t *testing.T) {
	for _, tc := range []struct {
		status   string
		hasError bool
		wantSt   string
		wantCode string
	}{
		{"SUCCESS", false, "completed", ""},
		{"SUCCESS", true, "failed", antigravityTurnFailed},
		{"CANCELED", false, "failed", antigravityCanceled},
		{"INTERRUPTED", false, "failed", antigravityInterrupted},
		{"ERROR", false, "failed", antigravityTurnFailed},
		{"INVALID", false, "failed", antigravityInvalid},
		{"SOMETHING_NEW", false, "failed", antigravityStatusUnrecognized},
	} {
		st, code := antigravityOutcome(tc.status, tc.hasError)
		if st != tc.wantSt || code != tc.wantCode {
			t.Errorf("%+v: got (%q, %q), want (%q, %q)", tc, st, code, tc.wantSt, tc.wantCode)
		}
	}

	u := parseAntigravityUsage(json.RawMessage(`{"input_tokens":100,"output_tokens":20,"thinking_tokens":5,"cache_read_tokens":300}`))
	if !u.Known || u.Input != 400 || u.Output != 25 || u.CacheRead != 300 || u.Reasoning != 5 || !u.CacheKnown || !u.ReasoningKnown {
		t.Fatalf("unexpected usage: %+v", u)
	}
	for _, raw := range []string{
		``,
		`{}`,
		`{"input_tokens":10}`,
		`{"input_tokens":-1,"output_tokens":1}`,
		`{"input_tokens":9223372036854775807,"output_tokens":1,"cache_read_tokens":1}`,
		`{"input_tokens":1,"output_tokens":9223372036854775807,"thinking_tokens":1}`,
	} {
		if bad := parseAntigravityUsage(json.RawMessage(raw)); bad.Known {
			t.Errorf("%s read as known: %+v", raw, bad)
		}
	}
}

func TestAntigravityRefusalsAndPolicy(t *testing.T) {
	workDir := t.TempDir()
	base := Options{Provider: harness.Provider{Engine: harness.Antigravity}, WorkDir: workDir}
	n, err := normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	if n.Provider.CLI.Binary != "agy" || n.Policy.AntigravityPermission != AntigravityRequestReview {
		t.Fatalf("defaults: %+v", n)
	}

	for name, mutate := range map[string]func(*Options){
		"custom home": func(o *Options) { o.Provider.CLI.Home = t.TempDir() },
		"bad permission": func(o *Options) {
			o.Policy.AntigravityPermission = "bypassPermissions"
		},
		"codex policy on agy": func(o *Options) {
			o.Policy.CodexSandbox = "read-only"
		},
		"claude policy on agy": func(o *Options) {
			o.Policy.ClaudePermission = "dontAsk"
		},
		"grok policy on agy": func(o *Options) {
			o.Policy.GrokPermission = GrokDenyWhenAsked
		},
		"instructions": func(o *Options) {
			o.Instructions = Instructions{Mode: Append, Text: "extra"}
		},
		"provided skills": func(o *Options) {
			o.Skills = harness.Skills{Provided: []harness.Skill{{Name: "s", Dir: workDir}}}
		},
		"exclude global skills": func(o *Options) {
			o.Skills = harness.Skills{Global: harness.GlobalSkillsExclude}
		},
		"restriction": func(o *Options) {
			o.Restriction = &Restriction{}
		},
		"sandbox": func(o *Options) {
			o.Sandbox = &Sandbox{}
		},
		"managed env ANTIGRAVITY_": func(o *Options) {
			o.Env = []string{"ANTIGRAVITY_CLI_ALIAS=agy"}
		},
		"managed env GEMINI_": func(o *Options) {
			o.Env = []string{"GEMINI_API_KEY=secret"}
		},
		"managed env JETSKI_": func(o *Options) {
			o.Env = []string{"JETSKI_ENABLE_TELEMETRY=1"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			if _, err := normalize(o); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
	if !policySet(Policy{AntigravityPermission: AntigravityRequestReview}) {
		t.Fatal("API session policySet ignored AntigravityPermission")
	}
}

func TestPersistedAntigravityRefIsStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pinned paths are Unix paths")
	}
	o, err := normalize(Options{
		Provider: harness.Provider{Engine: harness.Antigravity},
		Model:    "gemini-3.1-pro-high",
		Effort:   "low",
		WorkDir:  "/w",
		Policy:   Policy{AntigravityPermission: AntigravityAcceptEdits},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Replace the dynamic user home directory with a fixed path for the golden check.
	o.Provider.CLI.Home = "/h"
	got, _ := json.Marshal(reference(o, "s1"))
	want := `{"engine":"antigravity","id":"s1","home":"/h","work_dir":"/w","config_hash":"c8e3b76362441a2856536bd56d881e2029659bd2efdadfc1d350bfee47aa722c"}`
	if string(got) != want {
		t.Fatalf("persisted ref changed:\n got %s\nwant %s", got, want)
	}
}
