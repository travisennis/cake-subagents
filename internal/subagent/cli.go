package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Main is the process entry point. Cake invokes the `subagent` symlink; the
// only other public entry point is the internal v2-supervisor re-exec.
func Main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
		os.Exit(2)
	}
}

func run() error {
	arguments := append([]string(nil), os.Args...)
	program := filepath.Base(arguments[0])
	// Explicit subcommands win over filename dispatch: the supervisor re-execs
	// this binary, and on macOS os.Executable reports the symlink path it was
	// started from.
	if len(arguments) > 1 && arguments[1] == "v2-supervisor" {
		if len(arguments) < 3 {
			return errors.New("v2-supervisor requires a child_id")
		}
		return v2Supervisor(arguments[2])
	}
	// `event` and `hooks` are explicit subcommands, not Cake tools. They must
	// work through the dist binary as well as the subagent symlink and must not
	// depend on TOOLBOX_ACTION.
	if len(arguments) > 1 {
		switch arguments[1] {
		case "event":
			if len(arguments) != 2 {
				return errors.New("event takes no arguments")
			}
			return v2HookEvent(os.Stdin, os.Stdout)
		case "hooks":
			return v2HooksCommand(arguments[2:], os.Stdout)
		}
	}
	mode := ""
	if program == "subagent" {
		mode = "subagent"
	} else if len(arguments) > 1 && arguments[1] == "subagent" {
		mode = "subagent"
	}
	switch os.Getenv("TOOLBOX_ACTION") {
	case "describe":
		if mode != "subagent" {
			return usageError(program)
		}
		value, err := describeV2()
		if err != nil {
			return err
		}
		return printJSON(value)
	case "execute":
		if mode != "subagent" {
			return usageError(program)
		}
		value, err := decodeV2Arguments()
		if err != nil {
			return err
		}
		result, err := v2Execute(value)
		if err != nil {
			return err
		}
		return printJSON(result)
	}
	if mode == "" {
		return usageError(program)
	}
	return errors.New("TOOLBOX_ACTION must be describe or execute")
}

func usageError(program string) error {
	return fmt.Errorf("usage: %s (subagent|v2-supervisor)", program)
}

func decodeV2Arguments() (map[string]any, error) {
	decoder := json.NewDecoder(os.Stdin)
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("arguments must be a JSON object")
	}
	value, ok := raw.(map[string]any)
	if !ok || value == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return value, nil
}

func printJSON(value any) error {
	return printJSONTo(os.Stdout, value)
}

func printJSONTo(writer io.Writer, value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}
