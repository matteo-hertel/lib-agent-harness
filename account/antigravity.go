package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/process"
)

// antigravityTimeout bounds the whole inspection across /credits and /usage.
var antigravityTimeout = 30 * time.Second

const antigravityOutputLimit = 4 << 20

// inspectAntigravity runs `agy -p=/credits --output-format json` and
// `agy -p=/usage --output-format json` in a disposable directory. Neither
// slash command starts a model turn or creates a conversation.
func inspectAntigravity(ctx context.Context, p harness.Provider, binary string) (harness.AccountReport, error) {
	report := unknownReport(p.Engine, "not inspected")
	env, code := nativecli.AntigravityEnvironment(p.CLI.Home)
	if code != "" {
		family := harness.FailurePreflight
		if code == "antigravity_home_unsupported" {
			family = harness.FailureCapability
		}
		return report, &Error{Engine: p.Engine, Code: code, Family: family}
	}
	dir, err := os.MkdirTemp("", "agent-harness-account-")
	if err != nil {
		return report, &Error{Engine: p.Engine, Code: CodeTransport, Family: harness.FailureProcess}
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(ctx, antigravityTimeout)
	defer cancel()

	creditsRaw, err := antigravityCommand(ctx, binary, dir, env, "/credits")
	if err != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return report, ctx.Err()
		}
		return report, antigravityError(err)
	}
	account, credits, loggedIn, err := parseAntigravityCredits(creditsRaw)
	if err != nil {
		return report, antigravityError(err)
	}
	report.Account = account
	if !loggedIn {
		report.Quota.Reason = "not logged in"
		report.Credits.Reason = "not logged in"
		return report, nil
	}
	report.Credits = credits

	usageRaw, err := antigravityCommand(ctx, binary, dir, env, "/usage")
	if err != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return report, ctx.Err()
		}
		return report, antigravityError(err)
	}
	quota, err := parseAntigravityUsage(usageRaw)
	if err != nil {
		return report, antigravityError(err)
	}
	report.Quota = quota
	return explained(report, "not reported by the CLI"), nil
}

func antigravityCommand(ctx context.Context, binary, dir string, env []string, slash string) ([]byte, error) {
	cmd, proc, err := process.Command(ctx, binary, "-p="+slash, "--output-format", "json")
	if err != nil {
		return nil, errAntigravityStart
	}
	defer proc.Close()
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second

	reader, writer := io.Pipe()
	cmd.Stdout = writer
	reaped := make(chan error, 1)
	go func() {
		err := proc.Run()
		writer.CloseWithError(io.EOF)
		reaped <- err
	}()
	stopClosing := context.AfterFunc(ctx, func() {
		reader.CloseWithError(ctx.Err())
	})
	defer stopClosing()

	data, readErr := readBoundedAntigravity(reader, antigravityOutputLimit)
	_ = reader.Close()
	exit := <-reaped

	if err := ctx.Err(); err != nil && (readErr != nil || len(bytes.TrimSpace(data)) == 0) {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, antigravityExit(exit)
	}
	return data, nil
}

func readBoundedAntigravity(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	var probe [1]byte
	copied, err := io.CopyN(&buf, r, limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if copied == limit {
		if n, _ := r.Read(probe[:]); n > 0 {
			return nil, errAntigravityInvalid
		}
	}
	return buf.Bytes(), nil
}

type antigravityEnvelope struct {
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Command *struct {
		Name string          `json:"name"`
		Data json.RawMessage `json:"data"`
	} `json:"command"`
}

func parseAntigravityEnvelope(raw []byte, wantCommand string) (json.RawMessage, bool, error) {
	var env antigravityEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return nil, false, errAntigravityInvalid
	}
	if env.Status == "ERROR" || env.Error != nil {
		msg := ""
		if env.Error != nil {
			msg = strings.ToLower(env.Error.Message)
		}
		if strings.Contains(msg, "not logged in") || strings.Contains(msg, "sign in") || strings.Contains(msg, "unauthenticated") || strings.Contains(msg, "login required") {
			return nil, true, nil
		}
		if strings.Contains(msg, "unknown command") || strings.Contains(msg, "unrecognized command") {
			return nil, false, errAntigravityMethod
		}
		return nil, false, errAntigravityRejected
	}
	if env.Status != "SUCCESS" || env.Command == nil || env.Command.Name != wantCommand || len(env.Command.Data) == 0 {
		return nil, false, errAntigravityInvalid
	}
	return env.Command.Data, false, nil
}

func parseAntigravityCredits(raw []byte) (harness.AccountSnapshot, harness.CreditSnapshot, bool, error) {
	data, loggedOut, err := parseAntigravityEnvelope(raw, "credits")
	now := time.Now().UTC()
	if err != nil {
		return harness.AccountSnapshot{}, harness.CreditSnapshot{}, false, err
	}
	if loggedOut {
		loggedIn := false
		return harness.AccountSnapshot{
			Observation: harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/credits"},
			LoggedIn:    &loggedIn,
		}, harness.CreditSnapshot{}, false, nil
	}
	var payload struct {
		RemainingCredits *int64 `json:"remaining_credits"`
		PlanName         string `json:"plan_name"`
	}
	if json.Unmarshal(data, &payload) != nil || (payload.PlanName == "" && payload.RemainingCredits == nil) {
		return harness.AccountSnapshot{}, harness.CreditSnapshot{}, false, errAntigravityInvalid
	}
	loggedIn := true
	account := harness.AccountSnapshot{
		Observation: harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/credits"},
		LoggedIn:    &loggedIn,
		Plan:        payload.PlanName,
	}
	var credits harness.CreditSnapshot
	if payload.RemainingCredits != nil {
		enabled := true
		unlimited := false
		credits = harness.CreditSnapshot{
			Observation: harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/credits"},
			Enabled:     &enabled,
			Unlimited:   &unlimited,
			Balance: &harness.Amount{
				Value: strconv.FormatInt(*payload.RemainingCredits, 10),
				Unit:  harness.CreditUnit,
			},
		}
	} else {
		credits.Reason = "not reported by the CLI"
	}
	return account, credits, true, nil
}

func parseAntigravityUsage(raw []byte) (harness.QuotaSnapshot, error) {
	data, loggedOut, err := parseAntigravityEnvelope(raw, "usage")
	if err != nil {
		return harness.QuotaSnapshot{}, err
	}
	if loggedOut {
		return harness.QuotaSnapshot{}, errAntigravityRejected
	}
	var payload struct {
		Groups []struct {
			Name    string `json:"name"`
			Buckets []struct {
				Name              string   `json:"name"`
				Window            string   `json:"window"`
				Disabled          bool     `json:"disabled"`
				RemainingFraction *float64 `json:"remaining_fraction"`
				ResetTime         string   `json:"reset_time"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Groups == nil {
		return harness.QuotaSnapshot{}, errAntigravityInvalid
	}
	now := time.Now().UTC()
	obs := harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/usage"}
	var windows []harness.QuotaWindow
	for _, group := range payload.Groups {
		for _, bucket := range group.Buckets {
			if bucket.Disabled || bucket.RemainingFraction == nil {
				continue
			}
			rem := *bucket.RemainingFraction
			if math.IsNaN(rem) || math.IsInf(rem, 0) || rem < 0 || rem > 1 {
				return harness.QuotaSnapshot{}, errAntigravityInvalid
			}
			used := math.Round((1-rem)*10000) / 100
			if used < 0 {
				used = 0
			}
			model := bucket.Name
			if group.Name != "" && !strings.EqualFold(group.Name, "Models") {
				model = group.Name
			}
			window := harness.QuotaWindow{
				Observation: obs,
				Model:       model,
				Name:        bucket.Name,
				UsedPercent: &used,
			}
			switch bucket.Window {
			case "5h":
				mins := int64(300)
				window.ID = "5h"
				window.Kind = harness.QuotaSession
				window.WindowMinutes = &mins
			case "weekly":
				mins := int64(10080)
				window.ID = "weekly"
				window.Kind = harness.QuotaWeekly
				if bucket.Name != "" {
					window.Kind = harness.QuotaWeeklyModel
				}
				window.WindowMinutes = &mins
			default:
				window.Kind = harness.QuotaOther
			}
			if bucket.ResetTime != "" {
				parsed, err := time.Parse(time.RFC3339Nano, bucket.ResetTime)
				if err != nil {
					return harness.QuotaSnapshot{}, errAntigravityInvalid
				}
				utc := parsed.UTC()
				window.ResetsAt = &utc
			}
			windows = append(windows, window)
		}
	}
	return harness.QuotaSnapshot{
		Observation: obs,
		Complete:    true,
		Windows:     windows,
	}, nil
}

var (
	errAntigravityStart    = errors.New("antigravity could not start")
	errAntigravityInvalid  = errors.New("antigravity answered in an unrecognized shape")
	errAntigravityMethod   = errors.New("antigravity does not offer the method")
	errAntigravityRejected = errors.New("antigravity refused the request")
)

func antigravityExit(err error) error {
	var status *exec.ExitError
	switch {
	case errors.As(err, &status) && status.ExitCode() >= 0:
		code := status.ExitCode()
		return &Error{Engine: harness.Antigravity, Code: CodeProcessExited, Family: harness.FailureProcess, ExitCode: &code}
	case errors.As(err, &status):
		return &Error{Engine: harness.Antigravity, Code: CodeProcessSignalled, Family: harness.FailureProcess}
	case err != nil:
		return &Error{Engine: harness.Antigravity, Code: CodeProcessStartFailed, Family: harness.FailureProcess}
	}
	return &Error{Engine: harness.Antigravity, Code: CodeProcessExited, Family: harness.FailureProcess}
}

func antigravityError(err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	e := &Error{Engine: harness.Antigravity, Family: harness.FailureRequest}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.Code, e.Cause = CodeTimedOut, harness.CauseTimeout
	case errors.Is(err, errAntigravityMethod):
		e.Code, e.Family = CodeMethodUnavailable, harness.FailureCapability
	case errors.Is(err, errAntigravityRejected):
		e.Code, e.Cause = CodeRejected, harness.CauseUnknown
	case errors.Is(err, errAntigravityStart):
		e.Code, e.Family = CodeProcessStartFailed, harness.FailureProcess
	default:
		e.Code = CodeInvalidResponse
	}
	return e
}
