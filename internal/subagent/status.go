package subagent

import (
	"os"
	"path/filepath"
	"time"
)

func snapshot(directory string) (map[string]any, error) {
	state, err := readJSON(filepath.Join(directory, "status.json"))
	if err != nil {
		return nil, err
	}
	if activeStates[stringField(state, "status")] {
		lease, err := os.OpenFile(filepath.Join(directory, "worker.lock"), os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		if err := flock(lease, true); err == nil {
			// Re-read after acquiring the lease: the worker may just have finished.
			state, err = readJSON(filepath.Join(directory, "status.json"))
			if err != nil {
				lease.Close()
				return nil, err
			}
			if activeStates[stringField(state, "status")] {
				state["status"] = "unknown"
				state["error"] = "Supervisor exited without recording completion; Cake may still be running. Inspect the log and cake_pid before retrying."
			}
		}
		lease.Close()
	}
	if activeStates[stringField(state, "status")] {
		if _, err := os.Stat(filepath.Join(directory, "cancel")); err == nil {
			state["cancel_requested"] = true
		}
	}
	return state, nil
}

func status(args map[string]any) (map[string]any, error) {
	directory, err := runDirectory(stringField(args, "run_id"))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(intArg(args, "wait_seconds", 0)) * time.Second)
	for {
		state, err := snapshot(directory)
		if err != nil {
			return nil, err
		}
		if !activeStates[stringField(state, "status")] || time.Now().After(deadline) {
			return state, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func cancel(args map[string]any) (map[string]any, error) {
	directory, err := runDirectory(stringField(args, "run_id"))
	if err != nil {
		return nil, err
	}
	state, err := snapshot(directory)
	if err != nil {
		return nil, err
	}
	if activeStates[stringField(state, "status")] {
		contents := "requested " + now() + "\n"
		if err := os.WriteFile(filepath.Join(directory, "cancel"), []byte(contents), 0o600); err != nil {
			return nil, err
		}
		state["cancel_requested"] = true
	}
	return state, nil
}
