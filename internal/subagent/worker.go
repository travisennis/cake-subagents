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
	"unicode/utf8"
)

type processExit struct {
	code     int
	signal   int
	signaled bool
}

func waitProcess(command *exec.Cmd) processExit {
	_ = command.Wait()
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return processExit{code: command.ProcessState.ExitCode()}
	}
	if status.Signaled() {
		return processExit{signal: int(status.Signal()), signaled: true}
	}
	return processExit{code: status.ExitStatus()}
}

func withEnv(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if _, replaced := overrides[key]; replaced {
			continue
		}
		environment = append(environment, entry)
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func truncateFile(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func commandStrings(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, errors.New("request command must be a non-empty array")
	}
	command := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, errors.New("request command must contain only strings")
		}
		command = append(command, text)
	}
	return command, nil
}

// worker supervises one detached Cake process. It runs in its own process
// group with the launcher's flock leases inherited as file descriptors 3 and 4.
func worker(directory string) error {
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

	lease := os.NewFile(3, "worker.lock")
	if lease == nil {
		return errors.New("missing worker lease")
	}
	defer lease.Close()

	state, err := readJSON(filepath.Join(directory, "status.json"))
	if err != nil {
		return err
	}
	var lock *os.File
	if stringField(state, "session_id") != "" {
		lock = os.NewFile(4, "session.lock")
	}
	request, err := readJSON(filepath.Join(directory, "request.json"))
	if err != nil {
		return err
	}
	state["worker_pid"] = os.Getpid()
	eventsPath := stringField(state, "events_file")
	if err := truncateFile(eventsPath); err != nil {
		return err
	}

	children := map[string]string{
		"CAKE_TOOLBOX_SUBAGENT_DEPTH": fmt.Sprintf("%d", intField(request, "depth")),
		"CAKE_SUBAGENT_RUN_ID":        stringField(state, "run_id"),
	}
	if value := stringField(state, "session_id"); value != "" {
		children["CAKE_SUBAGENT_SESSION_ID"] = value
	}
	if value := stringField(state, "name"); value != "" {
		children["CAKE_SUBAGENT_NAME"] = value
	}
	if value := stringField(state, "team"); value != "" {
		children["CAKE_SUBAGENT_TEAM"] = value
	}

	command, err := commandStrings(request["command"])
	if err != nil {
		return err
	}
	events, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	logOutput, err := os.OpenFile(stringField(state, "log_file"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		events.Close()
		return err
	}
	cake := exec.Command(command[0], command[1:]...)
	cake.Dir = stringField(state, "cwd")
	cake.Stdout = events
	cake.Stderr = logOutput
	cake.Env = withEnv(children)
	cake.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	closeOnExec(3)
	if lock != nil {
		closeOnExec(4)
	}
	if err := cake.Start(); err != nil {
		events.Close()
		logOutput.Close()
		releaseInputs(directory, lock)
		markFailed(state, execFailure(err, command[0]))
		return nil
	}
	events.Close()
	logOutput.Close()
	state["cake_pid"] = cake.Process.Pid
	if err := writeJSON(stringField(state, "status_file"), state); err != nil {
		return err
	}

	exitChannel := make(chan processExit, 1)
	go func() { exitChannel <- waitProcess(cake) }()

	completion, reason, exit, runErr := supervise(directory, state, &requestedStop, &lock, exitChannel)
	if runErr != nil {
		signalGroup(cake.Process.Pid, syscall.SIGKILL)
		<-exitChannel
		releaseInputs(directory, lock)
		markFailed(state, runErr)
		fmt.Fprintf(os.Stderr, "subagent_async worker: %v\n", runErr)
		return nil
	}
	releaseInputs(directory, lock)
	finish(state, completion, exit, reason)
	return nil
}

func supervise(directory string, state map[string]any, requestedStop *atomic.Bool, lock **os.File, exitChannel <-chan processExit) (map[string]any, string, processExit, error) {
	events, err := os.Open(stringField(state, "events_file"))
	if err != nil {
		return nil, "", processExit{}, err
	}
	defer events.Close()

	timeout := intField(state, "timeout_seconds")
	if timeout <= 0 {
		timeout = maxRuntime
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	nextUpdate := time.Now().Add(time.Second)
	var completion map[string]any
	reason := ""
	var killAt time.Time
	var pending []byte
	readBuffer := make([]byte, 65_536)
	var exited *processExit

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
					if err := handleEvent(line, state, &completion, lock, directory); err != nil {
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
			return nil, "", processExit{}, err
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
				return nil, "", processExit{}, err
			}
			break
		}

		cancelPath := filepath.Join(directory, "cancel")
		_, cancelErr := os.Stat(cancelPath)
		cancelled := requestedStop.Load() || cancelErr == nil
		if reason == "" && (cancelled || !time.Now().Before(deadline)) {
			if cancelled {
				reason = "cancelled"
			} else {
				reason = "timed_out"
			}
			state["status"] = "stopping"
			_ = writeJSON(stringField(state, "status_file"), state)
			signalGroup(int(intField(state, "cake_pid")), syscall.SIGTERM)
			killAt = time.Now().Add(3 * time.Second)
		}
		if !killAt.IsZero() && !time.Now().Before(killAt) {
			signalGroup(int(intField(state, "cake_pid")), syscall.SIGKILL)
			killAt = time.Time{}
		}
		if !time.Now().Before(nextUpdate) {
			_ = writeJSON(stringField(state, "status_file"), state)
			nextUpdate = time.Now().Add(time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return completion, reason, *exited, nil
}

func handleEvent(line []byte, state map[string]any, completion *map[string]any, lock **os.File, directory string) error {
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
	state["event_count"] = intField(state, "event_count") + 1
	state["last_event_type"] = record["type"]
	state["last_event_at"] = now()

	switch stringField(record, "type") {
	case "task_start":
		sessionID := stringField(record, "session_id")
		taskID := stringField(record, "task_id")
		if !isUUID(sessionID) || !isUUID(taskID) {
			return errors.New("invalid task_start identity")
		}
		currentSession := stringField(state, "session_id")
		if state["task_id"] != nil || (currentSession != "" && currentSession != sessionID) {
			return errors.New("unexpected session/task identity")
		}
		if *lock == nil {
			acquired, err := sessionLock(sessionID)
			if err != nil {
				return err
			}
			*lock = acquired
		}
		state["session_id"] = sessionID
		state["task_id"] = taskID
		if path := sessionFile(sessionID, stringField(state, "cwd")); path != nil {
			state["session_file"] = *path
		}
		if stringField(state, "status") != "stopping" {
			state["status"] = "running"
		}
		return writeJSON(stringField(state, "status_file"), state)
	case "task_complete":
		if state["task_id"] == nil ||
			stringField(record, "task_id") != stringField(state, "task_id") ||
			stringField(record, "session_id") != stringField(state, "session_id") {
			return errors.New("task_complete does not match this invocation")
		}
		*completion = record
	}
	return nil
}

func releaseInputs(directory string, lock *os.File) {
	for _, name := range []string{"request.json", "system-prompt.md"} {
		_ = os.Remove(filepath.Join(directory, name))
	}
	if lock != nil {
		lock.Close()
	}
}

// execFailure matches Ruby's Errno::ENOENT message shape, which the offline
// suite asserts on for a missing CAKE_BIN.
func execFailure(err error, path string) error {
	if errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("No such file or directory - %s", path)
	}
	return err
}

func markFailed(state map[string]any, err error) {
	state["status"] = "failed"
	state["error"] = err.Error()
	state["finished_at"] = now()
	_ = writeJSON(stringField(state, "result_file"), state)
	_ = writeJSON(stringField(state, "status_file"), compact(state))
}

func finish(state map[string]any, completion map[string]any, exit processExit, reason string) {
	state["finished_at"] = now()
	if exit.signaled {
		state["exit_code"] = nil
		state["signal"] = exit.signal
	} else {
		state["exit_code"] = exit.code
	}
	if completion != nil {
		for _, key := range []string{"subtype", "result", "error", "usage", "duration_ms", "turn_count", "tool_call_count"} {
			if value, present := completion[key]; present {
				state[key] = value
			}
		}
	}
	switch {
	case reason != "":
		state["status"] = reason
		if reason == "timed_out" {
			state["error"] = "Subagent exceeded its runtime limit"
		} else {
			state["error"] = "Cancellation requested"
		}
	case completion == nil:
		state["status"] = "failed"
		state["error"] = "Cake exited without a matching task_complete event; inspect log_file"
	case stringField(completion, "subtype") == "success" && !exit.signaled && exit.code == 0:
		state["status"] = "completed"
	default:
		if stringField(completion, "subtype") == "interrupted" {
			state["status"] = "cancelled"
		} else {
			state["status"] = "failed"
		}
		if value, present := state["error"]; !present || value == nil {
			state["error"] = fmt.Sprintf("Cake finished with %s (exit %d)", stringField(completion, "subtype"), exit.code)
		}
	}
	withCompletion := make(map[string]any, len(state)+1)
	for key, value := range state {
		withCompletion[key] = value
	}
	withCompletion["completion"] = completion
	_ = writeJSON(stringField(state, "result_file"), withCompletion)
	_ = writeJSON(stringField(state, "status_file"), compact(state))
}

// compact keeps the model-visible payload under the toolbox stdout budget while
// the full result stays on disk.
func compact(state map[string]any) map[string]any {
	compactState := make(map[string]any, len(state)+2)
	for key, value := range state {
		compactState[key] = value
	}
	for _, key := range []string{"result", "error"} {
		text, ok := compactState[key].(string)
		if !ok {
			continue
		}
		for {
			data, err := marshalJSON(compactState)
			if err != nil || len(data) <= compactBytes || utf8.RuneCountInString(text) <= 256 {
				break
			}
			runes := []rune(text)
			text = string(runes[:len(runes)/2])
			compactState[key] = text
			compactState[key+"_truncated"] = true
		}
	}
	return compactState
}
