package subagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Main is the process entry point. The executable's filename selects the tool
// mode when Cake invokes one of the subagent_* symlinks; otherwise the first
// argument must be a subcommand.
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
	// this binary as `worker`, and on macOS os.Executable reports the symlink
	// path it was started from.
	if len(arguments) > 1 && (arguments[1] == "worker" || arguments[1] == "drain") {
		if arguments[1] == "worker" {
			if len(arguments) < 3 {
				return errors.New("worker requires a run directory")
			}
			return worker(arguments[2])
		}
		decision, err := drain()
		if err != nil {
			return err
		}
		if len(decision) > 0 {
			return printJSON(decision)
		}
		return nil
	}
	if mode, known := toolboxModes[program]; known {
		arguments = append([]string{arguments[0], mode}, arguments[1:]...)
	}
	mode := ""
	if len(arguments) > 1 {
		mode = arguments[1]
	}

	switch os.Getenv("TOOLBOX_ACTION") {
	case "describe":
		value, err := describe(mode)
		if err != nil {
			return err
		}
		return printJSON(value)
	case "execute":
		value, err := decodeArguments(mode)
		if err != nil {
			return err
		}
		result, err := execute(mode, value)
		if err != nil {
			return err
		}
		return printJSON(result)
	}
	if mode == "" {
		return fmt.Errorf("usage: %s (%s|drain)", program, strings.Join([]string{"launch", "status", "cancel", "post", "inbox", "wait", "peers"}, "|"))
	}
	return errors.New("TOOLBOX_ACTION must be describe or execute")
}

func execute(mode string, args map[string]any) (map[string]any, error) {
	switch mode {
	case "launch":
		return launch(args)
	case "status":
		return status(args)
	case "cancel":
		return cancel(args)
	case "post":
		return post(args)
	case "inbox":
		return inbox(args)
	case "wait":
		return waitForMessage(args)
	case "peers":
		return peers(args)
	}
	return nil, errors.New("unknown tool mode")
}

func printJSON(value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}
