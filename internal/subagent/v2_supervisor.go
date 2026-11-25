package subagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type v2Turn struct {
	Outcome         string
	StoppedByParent bool
	Closed          bool
	AlwaysReport    bool
}

func v2Marker(directory, name string) bool {
	_, err := os.Stat(filepath.Join(directory, name))
	return err == nil
}

func v2ChildEnv(child *v2Child, runID string) map[string]string {
	environment := map[string]string{
		"CAKE_SUBAGENT_DEPTH":         "1",
		"CAKE_TOOLBOX_SUBAGENT_DEPTH": "1",
		"CAKE_SUBAGENT_CHILD_ID":      child.ChildID,
		"CAKE_SUBAGENT_PARENT_OWNER":  child.OwnerID,
		"CAKE_SUBAGENT_NAME":          child.Name,
		"CAKE_SUBAGENT_RUN_ID":        runID,
	}
	if child.SessionID != "" {
		environment["CAKE_SUBAGENT_SESSION_ID"] = child.SessionID
	}
	return environment
}

func v2CakeBinary() (string, error) {
	if value := os.Getenv("CAKE_BIN"); value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "bin/cake")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	found, err := exec.LookPath("cake")
	if err != nil {
		return "", err
	}
	return found, nil
}

func v2BuildCommand(child *v2Child, prompt, runDirectory string) ([]string, error) {
	cakeBin, err := v2CakeBinary()
	if err != nil {
		return nil, err
	}
	command := []string{cakeBin, "--output-format", "stream-json", "--sandbox", child.Sandbox}
	if child.Model != "" {
		command = append(command, "--model", child.Model)
	}
	if child.Profile != "" {
		command = append(command, "--profile", child.Profile)
	}
	if child.Effort != "" {
		command = append(command, "--reasoning-effort", child.Effort)
	}
	if child.SystemPrompt != "" {
		path := filepath.Join(runDirectory, "system-prompt.md")
		if err := os.WriteFile(path, []byte(child.SystemPrompt), 0o600); err != nil {
			return nil, err
		}
		command = append(command, "--system-prompt", path)
	}
	if child.SessionID != "" {
		command = append(command, "--allow-session-model-change", "--resume", child.SessionID)
	}
	command = append(command, "--", prompt)
	return command, nil
}

func v2Supervisor(childID string) error {
	_ = syscall.Umask(0o077)
	ignoreHangup()
	var requestedStop atomic.Bool
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	go func() {
		for range signals {
			requestedStop.Store(true)
		}
	}()

	lease := os.NewFile(3, "supervisor.lock")
	if lease == nil {
		return errors.New("missing supervisor lease")
	}
	defer lease.Close()

	var sessionLock *os.File
	defer func() {
		if sessionLock != nil {
			sessionLock.Close()
		}
	}()
	if os.Getenv("CAKE_SUBAGENT_SESSION_LOCK") == "1" {
		sessionLock = os.NewFile(4, "session.lock")
		if sessionLock == nil {
			return errors.New("missing session lock")
		}
	}

	child, err := v2ReadChild(childID)
	if err != nil {
		return err
	}
	if child.State == "closed" {
		return nil
	}
	if sessionLock == nil && child.SessionID != "" {
		sessionLock, err = v2SessionLock(child.SessionID)
		if err != nil {
			return v2StartupFailure(child, err)
		}
	}
	child, err = v2UpdateChild(childID, func(record *v2Child) error {
		record.State = "starting"
		record.SupervisorPID = os.Getpid()
		return nil
	})
	if err != nil {
		return err
	}

	for {
		directory, err := v2ChildDir(childID)
		if err != nil {
			return err
		}
		if v2Marker(directory, "close") {
			return v2FinishClose(childID, child.Name)
		}
		if v2Marker(directory, "stop") {
			_, err := v2FinishStop(childID)
			return err
		}
		message, err := v2Dequeue(childID)
		if err != nil {
			return err
		}
		if message == nil {
			break
		}
		if v2Marker(directory, "close") {
			return v2FinishClose(childID, child.Name)
		}
		if v2Marker(directory, "stop") {
			_, err := v2FinishStop(childID)
			return err
		}
		child, err = v2ReadChild(childID)
		if err != nil {
			return err
		}
		turn, err := v2RunTurn(child, message, &sessionLock, &requestedStop)
		if err != nil {
			return err
		}
		if turn.Closed {
			return v2FinishClose(childID, child.Name)
		}
		if turn.StoppedByParent {
			if v2Marker(directory, "close") {
				return v2FinishClose(childID, child.Name)
			}
			_, err := v2FinishStop(childID)
			return err
		}
	}

	if _, err := v2UpdateChild(childID, func(record *v2Child) error {
		if record.State != "closed" {
			record.State = "idle"
		}
		record.SupervisorPID = 0
		return nil
	}); err != nil {
		return err
	}

	// Handoff: release the lease, then check once without it. A message that
	// arrived during the exit window either sees the queue here or acquires the
	// released lease and starts its own supervisor. Never reacquire before this
	// check, or a message enqueued between the check and exit can be stranded.
	if err := unlock(lease); err != nil {
		return err
	}
	directory, err := v2ChildDir(childID)
	if err != nil {
		return err
	}
	if v2Marker(directory, "close") || v2Marker(directory, "stop") {
		if err := flock(lease, true); err == nil {
			if v2Marker(directory, "close") {
				return v2FinishClose(childID, child.Name)
			}
			_, err := v2FinishStop(childID)
			return err
		}
		return nil
	}
	count, err := v2QueueCount(childID)
	if err != nil {
		return err
	}
	if count > 0 {
		if err := flock(lease, true); err == nil {
			if _, err := v2UpdateChild(childID, func(record *v2Child) error {
				record.State = "starting"
				record.SupervisorPID = os.Getpid()
				return nil
			}); err != nil {
				return err
			}
			return v2SupervisorContinue(childID, lease, sessionLock, &requestedStop, child)
		}
	}
	return nil
}

// v2SupervisorContinue runs the same loop after the exit handoff reacquired
// the lease. It exists so the lease and acquired session lock stay the same
// objects across the handoff.
func v2SupervisorContinue(childID string, lease, sessionLock *os.File, requestedStop *atomic.Bool, child *v2Child) error {
	defer func() {
		if sessionLock != nil {
			sessionLock.Close()
		}
	}()
	for {
		directory, err := v2ChildDir(childID)
		if err != nil {
			return err
		}
		if v2Marker(directory, "close") {
			return v2FinishClose(childID, child.Name)
		}
		if v2Marker(directory, "stop") {
			_, err := v2FinishStop(childID)
			return err
		}
		message, err := v2Dequeue(childID)
		if err != nil {
			return err
		}
		if message == nil {
			break
		}
		child, err = v2ReadChild(childID)
		if err != nil {
			return err
		}
		turn, err := v2RunTurn(child, message, &sessionLock, requestedStop)
		if err != nil {
			return err
		}
		if turn.Closed {
			return v2FinishClose(childID, child.Name)
		}
		if turn.StoppedByParent {
			if v2Marker(directory, "close") {
				return v2FinishClose(childID, child.Name)
			}
			_, err := v2FinishStop(childID)
			return err
		}
	}
	if _, err := v2UpdateChild(childID, func(record *v2Child) error {
		if record.State != "closed" {
			record.State = "idle"
		}
		record.SupervisorPID = 0
		return nil
	}); err != nil {
		return err
	}
	if err := unlock(lease); err != nil {
		return err
	}
	directory, err := v2ChildDir(childID)
	if err != nil {
		return err
	}
	if v2Marker(directory, "close") || v2Marker(directory, "stop") {
		if err := flock(lease, true); err == nil {
			if v2Marker(directory, "close") {
				return v2FinishClose(childID, child.Name)
			}
			_, err := v2FinishStop(childID)
			return err
		}
		return nil
	}
	count, err := v2QueueCount(childID)
	if err != nil {
		return err
	}
	if count > 0 {
		if err := flock(lease, true); err == nil {
			if _, err := v2UpdateChild(childID, func(record *v2Child) error {
				record.State = "starting"
				record.SupervisorPID = os.Getpid()
				return nil
			}); err != nil {
				return err
			}
			return v2SupervisorContinue(childID, lease, sessionLock, requestedStop, child)
		}
	}
	return nil
}

func v2RunTurn(child *v2Child, message *v2QueuedMessage, sessionLock **os.File, requestedStop *atomic.Bool) (v2Turn, error) {
	turn := v2Turn{}
	runID, err := newUUID()
	if err != nil {
		return turn, err
	}
	childDirectory, err := v2ChildDir(child.ChildID)
	if err != nil {
		return turn, err
	}
	runDirectory := filepath.Join(childDirectory, "runs", runID)
	if err := v2EnsureDir(runDirectory); err != nil {
		return turn, err
	}
	eventsPath := filepath.Join(runDirectory, "events.jsonl")
	logPath := filepath.Join(runDirectory, "stderr.log")
	resultPath := filepath.Join(runDirectory, "result.json")
	statusPath := filepath.Join(runDirectory, "status.json")
	messagePath := filepath.Join(runDirectory, "message.json")
	run := map[string]any{
		"run_id": runID, "child_id": child.ChildID, "status": "starting",
		"cwd": child.Cwd, "type": child.Type, "started_at": now(),
		"timeout_seconds": message.TimeoutSeconds, "status_file": statusPath,
		"events_file": eventsPath, "result_file": resultPath, "log_file": logPath,
		"message_file": messagePath,
	}
	if child.SessionID != "" {
		run["session_id"] = child.SessionID
	}
	if err := writeJSON(messagePath, message); err != nil {
		return turn, err
	}
	if err := writeJSON(statusPath, run); err != nil {
		return turn, err
	}
	if _, err := v2UpdateChild(child.ChildID, func(record *v2Child) error {
		if record.State != "stopping" && record.State != "closed" {
			record.State = "starting"
		}
		record.LastRunID = runID
		return nil
	}); err != nil {
		return turn, err
	}

	command, err := v2BuildCommand(child, message.Prompt, runDirectory)
	if err != nil {
		return v2FailTurn(child, run, execFailure(err, "cake"), true)
	}
	events, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return turn, err
	}
	logOutput, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		events.Close()
		return turn, err
	}
	cake := exec.Command(command[0], command[1:]...)
	cake.Dir = child.Cwd
	cake.Stdout = events
	cake.Stderr = logOutput
	cake.Env = withEnv(v2ChildEnv(child, runID))
	cake.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	closeOnExec(3)
	if *sessionLock != nil {
		closeOnExec(4)
	}
	if err := cake.Start(); err != nil {
		events.Close()
		logOutput.Close()
		if child.SystemPrompt != "" {
			_ = os.Remove(filepath.Join(runDirectory, "system-prompt.md"))
		}
		return v2FailTurn(child, run, execFailure(err, command[0]), true)
	}
	events.Close()
	logOutput.Close()
	run["cake_pid"] = cake.Process.Pid
	_ = writeJSON(statusPath, run)

	exitChannel := make(chan processExit, 1)
	go func() { exitChannel <- waitProcess(cake) }()

	completion, reason, stoppedByParent, interrupted, exit, eventErr := v2SuperviseRun(child, run, requestedStop, sessionLock, exitChannel)
	if eventErr != nil {
		signalGroup(cake.Process.Pid, syscall.SIGKILL)
		<-exitChannel
		return v2FailTurn(child, run, eventErr, !stoppedByParent)
	}
	if err := v2FinishRun(child, run, completion, exit, reason, stoppedByParent, interrupted); err != nil {
		return turn, err
	}
	turn.Outcome = stringField(run, "status")
	turn.StoppedByParent = stoppedByParent
	turn.AlwaysReport = interrupted
	turn.Closed = v2Marker(runDirectory, "close") || v2Marker(childDirectory, "close")
	return turn, nil
}

func v2SuperviseRun(child *v2Child, run map[string]any, requestedStop *atomic.Bool, sessionLock **os.File, exitChannel <-chan processExit) (map[string]any, string, bool, bool, processExit, error) {
	events, err := os.Open(stringField(run, "events_file"))
	if err != nil {
		return nil, "", false, false, processExit{}, err
	}
	defer events.Close()

	timeout := intField(run, "timeout_seconds")
	if timeout <= 0 {
		timeout = v2DefaultRuntime
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	nextUpdate := time.Now().Add(time.Second)
	var completion map[string]any
	var reason string
	var stoppedByParent bool
	var interrupted bool
	var killAt time.Time
	var pending []byte
	readBuffer := make([]byte, 65_536)
	var exited *processExit
	directory, err := v2ChildDir(child.ChildID)
	if err != nil {
		return nil, "", false, false, processExit{}, err
	}

	drain := func() error {
		for {
			count, readErr := events.Read(readBuffer)
			if count > 0 {
				pending = append(pending, readBuffer[:count]...)
				for {
					index := bytes.IndexByte(pending, '\n')
					if index < 0 {
						break
					}
					line := pending[:index]
					pending = pending[index+1:]
					if err := v2HandleEvent(line, run, child, &completion, sessionLock); err != nil {
						return err
					}
				}
			}
			if readErr != nil {
				return nil
			}
		}
	}

	for {
		if err := drain(); err != nil {
			return nil, "", false, false, processExit{}, err
		}
		if exited != nil {
			break
		}
		select {
		case value := <-exitChannel:
			exited = &value
		default:
		}
		if exited != nil {
			if err := drain(); err != nil {
				return nil, "", false, false, processExit{}, err
			}
			break
		}
		if reason == "" {
			if v2Marker(directory, "close") {
				reason = "cancelled"
				stoppedByParent = true
			} else if v2Marker(directory, "stop") {
				reason = "cancelled"
				stoppedByParent = true
			} else if requestedStop.Load() {
				reason = "cancelled"
				interrupted = true
			} else if !time.Now().Before(deadline) {
				reason = "timed_out"
			}
			if reason != "" {
				run["status"] = "stopping"
				_ = writeJSON(stringField(run, "status_file"), run)
				signalGroup(int(intField(run, "cake_pid")), syscall.SIGTERM)
				killAt = time.Now().Add(3 * time.Second)
			}
		}
		if !killAt.IsZero() && !time.Now().Before(killAt) {
			signalGroup(int(intField(run, "cake_pid")), syscall.SIGKILL)
			killAt = time.Time{}
		}
		if !time.Now().Before(nextUpdate) {
			_ = writeJSON(stringField(run, "status_file"), run)
			nextUpdate = time.Now().Add(time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return completion, reason, stoppedByParent, interrupted, *exited, nil
}

func v2HandleEvent(line []byte, run map[string]any, child *v2Child, completion *map[string]any, sessionLock **os.File) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var record map[string]any
	if err := json.Unmarshal(line, &record); err != nil {
		return err
	}
	if record == nil {
		return errors.New("stream event must be a JSON object")
	}
	run["event_count"] = intField(run, "event_count") + 1
	run["last_event_type"] = record["type"]
	run["last_event_at"] = now()

	switch stringField(record, "type") {
	case "task_start":
		sessionID := stringField(record, "session_id")
		taskID := stringField(record, "task_id")
		if !isUUID(sessionID) || !isUUID(taskID) {
			return errors.New("invalid task_start identity")
		}
		if child.SessionID != "" && !strings.EqualFold(child.SessionID, sessionID) {
			return errors.New("unexpected session identity")
		}
		if run["task_id"] != nil {
			return errors.New("unexpected task_start")
		}
		if *sessionLock == nil {
			acquired, err := v2SessionLock(sessionID)
			if err != nil {
				return err
			}
			*sessionLock = acquired
		}
		child.SessionID = strings.ToLower(sessionID)
		_, _ = v2UpdateChild(child.ChildID, func(record *v2Child) error {
			record.SessionID = child.SessionID
			if record.State != "stopping" && record.State != "closed" {
				record.State = "running"
			}
			return nil
		})
		run["session_id"] = child.SessionID
		run["task_id"] = strings.ToLower(taskID)
		if value := stringField(record, "model"); value != "" {
			run["model"] = value
		}
		if value := stringField(record, "model_config"); value != "" {
			run["model_config"] = value
		}
		if sessionFile := sessionFile(child.SessionID, child.Cwd); sessionFile != nil {
			run["session_file"] = *sessionFile
		}
		if stringField(run, "status") != "stopping" {
			run["status"] = "running"
		}
		return writeJSON(stringField(run, "status_file"), run)
	case "task_complete":
		if run["task_id"] == nil ||
			!strings.EqualFold(stringField(record, "task_id"), asString(run["task_id"])) ||
			!strings.EqualFold(stringField(record, "session_id"), asString(run["session_id"])) {
			return errors.New("task_complete does not match this invocation")
		}
		*completion = record
	}
	return nil
}

func v2FinishRun(child *v2Child, run map[string]any, completion map[string]any, exit processExit, reason string, stoppedByParent, interrupted bool) error {
	outcome := "failed"
	alwaysReport := false
	errorText := ""
	resultText := ""
	if exit.signaled {
		run["exit_code"] = nil
		run["signal"] = exit.signal
	} else {
		run["exit_code"] = exit.code
	}
	if completion != nil {
		for _, key := range []string{"subtype", "result", "error", "usage", "duration_ms", "turn_count", "tool_call_count"} {
			if value, present := completion[key]; present {
				run[key] = value
			}
		}
		errorText = stringField(completion, "error")
		resultText = stringField(completion, "result")
	}
	switch {
	case reason == "timed_out":
		outcome = "timed_out"
		alwaysReport = true
		if errorText == "" {
			errorText = "Subagent exceeded its runtime limit"
		}
	case reason == "cancelled" && stoppedByParent:
		outcome = "cancelled"
	case reason == "cancelled":
		outcome = "cancelled"
		alwaysReport = true
	case interrupted:
		outcome = "cancelled"
		alwaysReport = true
	case completion == nil:
		outcome = "failed"
		alwaysReport = true
		if errorText == "" {
			errorText = "Cake exited without a matching task_complete event; inspect log_file"
			if tail := logTail(stringField(run, "log_file"), 500); tail != "" {
				errorText = "Cake exited without a matching task_complete event: " + tail
			}
		}
	case stringField(completion, "subtype") == "success" && !exit.signaled && exit.code == 0:
		outcome = "completed"
	default:
		alwaysReport = true
		outcome = "failed"
		if stringField(completion, "subtype") == "interrupted" {
			outcome = "cancelled"
		}
		if errorText == "" {
			errorText = fmt.Sprintf("Cake finished with %s (exit %d)", stringField(completion, "subtype"), exit.code)
		}
	}
	run["status"] = outcome
	run["finished_at"] = now()
	if errorText != "" {
		run["error"] = errorText
	}
	if resultText != "" {
		run["result"] = resultText
	}
	_ = writeJSON(stringField(run, "result_file"), run)
	_ = writeJSON(stringField(run, "status_file"), compact(run))
	if child.SystemPrompt != "" {
		_ = os.Remove(filepath.Join(filepath.Dir(stringField(run, "status_file")), "system-prompt.md"))
	}
	child, err := v2UpdateChild(child.ChildID, func(record *v2Child) error {
		record.SessionID = child.SessionID
		record.LastOutcome = outcome
		record.LastRunID = stringField(run, "run_id")
		if record.State != "closed" && record.State != "stopping" {
			record.State = "running"
		}
		return nil
	})
	if err != nil {
		return err
	}
	if v2ReportOwed(child, outcome, stoppedByParent, alwaysReport) {
		return v2WriteReport(child, run, outcome, resultText, errorText)
	}
	return nil
}

func v2FailTurn(child *v2Child, run map[string]any, failure error, alwaysReport bool) (v2Turn, error) {
	errorText := failure.Error()
	run["status"] = "failed"
	run["finished_at"] = now()
	run["error"] = errorText
	_ = writeJSON(stringField(run, "result_file"), run)
	_ = writeJSON(stringField(run, "status_file"), compact(run))
	if child.SystemPrompt != "" {
		_ = os.Remove(filepath.Join(filepath.Dir(stringField(run, "status_file")), "system-prompt.md"))
	}
	updated, err := v2UpdateChild(child.ChildID, func(record *v2Child) error {
		record.SessionID = child.SessionID
		record.LastOutcome = "failed"
		record.LastRunID = stringField(run, "run_id")
		if record.State != "closed" && record.State != "stopping" {
			record.State = "running"
		}
		return nil
	})
	if err != nil {
		return v2Turn{}, err
	}
	if alwaysReport && v2ReportOwed(updated, "failed", false, true) {
		if err := v2WriteReport(updated, run, "failed", "", errorText); err != nil {
			return v2Turn{}, err
		}
	}
	return v2Turn{Outcome: "failed", AlwaysReport: alwaysReport}, nil
}

func v2StartupFailure(child *v2Child, failure error) error {
	updated, err := v2UpdateChild(child.ChildID, func(record *v2Child) error {
		if record.State != "closed" {
			record.State = "idle"
		}
		record.LastOutcome = "failed"
		record.SupervisorPID = 0
		return nil
	})
	if err != nil {
		return err
	}
	_, err = v2AppendInbox(updated.OwnerID, map[string]any{
		"kind": "report", "agent": updated.Name, "child_id": updated.ChildID,
		"run_id": "", "session_id": updated.SessionID, "outcome": "failed",
		"report": updated.Report, "text": "", "error": failure.Error(),
		"created_at": now(), "report_file": "",
	})
	return err
}

func v2ReportOwed(child *v2Child, outcome string, stoppedByParent, alwaysReport bool) bool {
	if stoppedByParent {
		return false
	}
	if alwaysReport || outcome == "failed" || outcome == "timed_out" || outcome == "unknown" {
		return true
	}
	if child.Report == "never" {
		return false
	}
	return outcome == "completed" || outcome == "cancelled"
}

func v2WriteReport(child *v2Child, run map[string]any, outcome, text, errorText string) error {
	item := map[string]any{
		"kind": "report", "agent": child.Name, "child_id": child.ChildID,
		"run_id": stringField(run, "run_id"), "session_id": stringField(run, "session_id"),
		"outcome": outcome, "report": child.Report, "text": text, "error": errorText,
		"created_at": now(), "report_file": stringField(run, "result_file"),
	}
	_, err := v2AppendInbox(child.OwnerID, item)
	return err
}
