package subagent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func sessionLock(id string) (*os.File, error) {
	if id == "" {
		return nil, nil
	}
	directory := filepath.Join(runsRoot(), "locks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, id+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock(file, true); err != nil {
		file.Close()
		return nil, fmt.Errorf("session %s already has an active asynchronous run", id)
	}
	return file, nil
}

func launch(args map[string]any) (result map[string]any, err error) {
	depthText, present := os.LookupEnv("CAKE_TOOLBOX_SUBAGENT_DEPTH")
	if !present {
		depthText = "0"
	}
	if !depthPattern.MatchString(depthText) {
		return nil, errors.New("invalid CAKE_TOOLBOX_SUBAGENT_DEPTH")
	}
	depth, err := strconv.Atoi(depthText)
	if err != nil {
		return nil, errors.New("invalid CAKE_TOOLBOX_SUBAGENT_DEPTH")
	}
	if depth >= 1 {
		return nil, errors.New("nested delegation is not allowed")
	}

	cwdArg := stringField(args, "cwd")
	if cwdArg == "" {
		cwdArg, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	cwd, err := filepath.EvalSymlinks(cwdArg)
	if err != nil {
		return nil, err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return nil, errors.New("cwd must be a directory")
	}

	typeName := "general"
	if value := stringField(args, "type"); value != "" {
		typeName = value
	}
	var config *agentType
	if typeName != "general" {
		items, configErr := configs(cwd)
		if configErr != nil {
			return nil, configErr
		}
		for index := range items {
			if items[index].Name == typeName {
				config = &items[index]
				break
			}
		}
		if config == nil {
			return nil, fmt.Errorf("unknown subagent type %q", typeName)
		}
	}

	name := stringField(args, "name")
	if name != "" {
		for _, entry := range rosterEntries() {
			if stringField(entry, "name") == name && activeStates[stringField(entry, "status")] {
				return nil, fmt.Errorf("name %q is already in use by an active run", name)
			}
		}
	}

	sessionID := stringField(args, "session_id")
	var lock *os.File
	if sessionID != "" {
		lock, err = sessionLock(sessionID)
		if err != nil {
			return nil, err
		}
	}

	id, err := newUUID()
	if err != nil {
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	directory := filepath.Join(runsRoot(), id)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	lease, err := os.OpenFile(filepath.Join(directory, "worker.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	if err := flock(lease, false); err != nil {
		lease.Close()
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}

	spawned := false
	var state map[string]any
	defer func() {
		if lease != nil {
			lease.Close()
		}
		if lock != nil {
			lock.Close()
		}
		if err != nil && !spawned && state != nil {
			state["status"] = "failed"
			state["error"] = err.Error()
			state["finished_at"] = now()
			_ = writeJSON(stringField(state, "status_file"), state)
		}
	}()

	state = map[string]any{
		"run_id":          id,
		"status":          "starting",
		"session_id":      nil,
		"task_id":         nil,
		"session_file":    nil,
		"cwd":             cwd,
		"type":            typeName,
		"name":            nil,
		"team":            nil,
		"created_at":      now(),
		"timeout_seconds": intArg(args, "timeout_seconds", maxRuntime),
	}
	if sessionID != "" {
		state["session_id"] = sessionID
		if path := sessionFile(sessionID, cwd); path != nil {
			state["session_file"] = *path
		}
	}
	if name != "" {
		state["name"] = name
	}
	if team := stringField(args, "team"); team != "" {
		state["team"] = team
	}
	for key, file := range map[string]string{
		"status": "status.json", "events": "events.jsonl", "log": "stderr.log", "result": "result.json",
	} {
		state[key+"_file"] = filepath.Join(directory, file)
	}
	if err := ensureMailbox(directory); err != nil {
		return nil, err
	}
	if err := writeJSON(stringField(state, "status_file"), state); err != nil {
		return nil, err
	}
	if err := rosterWrite(map[string]any{
		"run_id": id, "session_id": state["session_id"], "name": state["name"], "team": state["team"],
		"cwd": cwd, "type": typeName, "status": "starting", "created_at": state["created_at"],
	}); err != nil {
		return nil, err
	}

	sandbox := "workspace-write"
	if value := stringField(args, "sandbox"); value != "" {
		sandbox = value
	}
	if config != nil {
		sandbox = config.Sandbox
	}
	cakeBin := os.Getenv("CAKE_BIN")
	if cakeBin == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, homeErr
		}
		cakeBin = filepath.Join(home, "bin/cake")
	}
	command := []string{
		cakeBin, "--model", "deepseek-flash-xhigh", "--profile", "all-skills",
		"--output-format", "stream-json", "--sandbox", sandbox,
	}
	if config != nil {
		promptPath := filepath.Join(directory, "system-prompt.md")
		if err := os.WriteFile(promptPath, []byte(config.Body), 0o600); err != nil {
			return nil, err
		}
		command = append(command, "--system-prompt", promptPath)
	}
	if sessionID != "" {
		command = append(command, "--resume", sessionID)
	}
	command = append(command, "--", stringField(args, "prompt"))
	request := map[string]any{"command": command, "depth": depth + 1}
	if err := writeJSON(filepath.Join(directory, "request.json"), request); err != nil {
		return nil, err
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(stringField(state, "log_file"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	process := exec.Command(executable, "worker", directory)
	process.Stderr = logFile
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.ExtraFiles = []*os.File{lease}
	if lock != nil {
		process.ExtraFiles = append(process.ExtraFiles, lock)
	}
	if err := process.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	logFile.Close()
	spawned = true
	lease.Close()
	lease = nil
	if lock != nil {
		lock.Close()
		lock = nil
	}

	deadline := time.Now().Add(startupWait)
	for {
		result, err = snapshot(directory)
		if err != nil {
			return nil, err
		}
		if stringField(result, "status") != "starting" || time.Now().After(deadline) {
			return result, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
