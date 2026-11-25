package subagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	v2HookScriptName       = "subagent-reports.sh"
	v2LegacyHookScriptName = "subagent-inbox.sh"
	v2HookTimeout          = 10
	// The literal matches Cake's own hook examples: the shell expands the
	// config directory at hook time, so the entry survives a config move.
	v2HookCommand = "\"${XDG_CONFIG_HOME:-$HOME/.config}/cake/hooks/subagent-reports.sh\""
)

// v2HooksCommand is the explicit `hooks` subcommand. It edits only the global
// hooks file and preserves every unrelated group and event.
func v2HooksCommand(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("hooks requires install, uninstall, or status")
	}
	switch args[0] {
	case "install":
		preToolUse := false
		for _, argument := range args[1:] {
			switch argument {
			case "--pre-tool-use":
				preToolUse = true
			default:
				return fmt.Errorf("unknown hooks install option %q", argument)
			}
		}
		result, err := v2InstallHooks(preToolUse)
		if err != nil {
			return err
		}
		return printJSONTo(stdout, result)
	case "uninstall":
		preToolUseOnly := false
		for _, argument := range args[1:] {
			switch argument {
			case "--pre-tool-use":
				preToolUseOnly = true
			default:
				return fmt.Errorf("unknown hooks uninstall option %q", argument)
			}
		}
		result, err := v2UninstallHooks(preToolUseOnly)
		if err != nil {
			return err
		}
		return printJSONTo(stdout, result)
	case "status":
		if len(args) > 1 {
			return fmt.Errorf("unknown hooks status option %q", args[1])
		}
		result, err := v2HooksStatus()
		if err != nil {
			return err
		}
		return printJSONTo(stdout, result)
	default:
		return fmt.Errorf("unknown hooks action %q", args[0])
	}
}

func v2HooksFilePath() (string, error) {
	// Match the shell entry point, which uses
	// "${XDG_CONFIG_HOME:-$HOME/.config}". os.UserConfigDir follows the macOS
	// Library/Application Support convention and would edit a different file
	// than Cake reads.
	if config := os.Getenv("XDG_CONFIG_HOME"); config != "" {
		return filepath.Join(config, "cake", "hooks.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cake", "hooks.json"), nil
}

func v2InstallHooks(preToolUse bool) (map[string]any, error) {
	path, err := v2HooksFilePath()
	if err != nil {
		return nil, err
	}
	document, existed, err := v2ReadHooksDocument(path)
	if err != nil {
		return nil, err
	}
	before, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	hooks := v2HooksMap(document)

	legacyRemoved := 0
	for _, event := range v2HookEventKeys(hooks) {
		groups, removed := v2FilterHookScripts(v2EventGroups(hooks, event), []string{v2LegacyHookScriptName})
		if removed > 0 {
			v2SetEventGroups(hooks, event, groups)
			legacyRemoved += removed
		}
	}

	// Our own entries are replaced so a stale command or timeout is repaired.
	// A PreToolUse entry is left alone unless the caller opts in or out; a
	// plain install must not silently remove an existing opt-in.
	for _, event := range []string{"SessionStart", "Stop"} {
		groups, _ := v2FilterHookScripts(v2EventGroups(hooks, event), []string{v2HookScriptName})
		v2SetEventGroups(hooks, event, groups)
	}
	if preToolUse {
		groups, _ := v2FilterHookScripts(v2EventGroups(hooks, "PreToolUse"), []string{v2HookScriptName})
		v2SetEventGroups(hooks, "PreToolUse", groups)
	}

	v2AppendHookGroup(hooks, "SessionStart", "")
	v2AppendHookGroup(hooks, "Stop", "")
	if preToolUse {
		v2AppendHookGroup(hooks, "PreToolUse", "*")
	}

	after, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	changed := !existed || !bytes.Equal(before, after)
	if changed {
		if err := v2WriteHooksDocument(path, document); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"path":           path,
		"changed":        changed,
		"created":        !existed,
		"installed":      v2InstalledHookEvents(hooks),
		"pre_tool_use":   v2HookScriptInstalled(hooks, "PreToolUse"),
		"legacy_removed": legacyRemoved,
	}, nil
}

func v2UninstallHooks(preToolUseOnly bool) (map[string]any, error) {
	path, err := v2HooksFilePath()
	if err != nil {
		return nil, err
	}
	document, existed, err := v2ReadHooksDocument(path)
	if err != nil {
		return nil, err
	}
	if !existed {
		return map[string]any{
			"path": path, "changed": false, "removed": 0, "installed": []string{},
		}, nil
	}
	before, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	hooks := v2HooksMap(document)
	events := v2HookEventKeys(hooks)
	if preToolUseOnly {
		events = []string{"PreToolUse"}
	}
	names := []string{v2HookScriptName}
	if !preToolUseOnly {
		names = append(names, v2LegacyHookScriptName)
	}
	removed := 0
	for _, event := range events {
		groups, count := v2FilterHookScripts(v2EventGroups(hooks, event), names)
		if count > 0 {
			v2SetEventGroups(hooks, event, groups)
			removed += count
		}
	}
	after, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	changed := !bytes.Equal(before, after)
	if changed {
		if err := v2WriteHooksDocument(path, document); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"path": path, "changed": changed, "removed": removed,
		"installed": v2InstalledHookEvents(hooks),
	}, nil
}

func v2HooksStatus() (map[string]any, error) {
	path, err := v2HooksFilePath()
	if err != nil {
		return nil, err
	}
	document, existed, err := v2ReadHooksDocument(path)
	if err != nil {
		return nil, err
	}
	installed := []string{}
	legacy := false
	if existed {
		hooks := v2HooksMap(document)
		installed = v2InstalledHookEvents(hooks)
		legacy = v2AnyHookScript(hooks, v2LegacyHookScriptName)
	}
	return map[string]any{
		"path":         path,
		"exists":       existed,
		"installed":    installed,
		"pre_tool_use": contains(installed, "PreToolUse"),
		"legacy":       legacy,
		"command":      v2HookCommand,
	}, nil
}

// v2ReadHooksDocument returns the global hooks document and whether it
// existed. A missing file yields a fresh version-1 document. The parser
// validates every event array and hook object it may rewrite so a malformed
// file is never replaced from an incomplete read.
func v2ReadHooksDocument(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"version": 1, "hooks": map[string]any{}}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, true, fmt.Errorf("%s: expected one JSON object", path)
	}
	version, ok := asInt64(document["version"])
	if !ok || version != 1 {
		return nil, true, fmt.Errorf("%s: hooks version must be 1", path)
	}
	raw, present := document["hooks"]
	if !present {
		document["hooks"] = map[string]any{}
		return document, true, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, true, fmt.Errorf("%s: hooks must be an object", path)
	}
	for event, value := range hooks {
		groups, ok := value.([]any)
		if !ok {
			return nil, true, fmt.Errorf("%s: hooks.%s must be an array", path, event)
		}
		for index, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				return nil, true, fmt.Errorf("%s: hooks.%s[%d] must be an object", path, event, index)
			}
			rawHooks, ok := group["hooks"].([]any)
			if !ok {
				return nil, true, fmt.Errorf("%s: hooks.%s[%d].hooks must be an array", path, event, index)
			}
			for hookIndex, rawHook := range rawHooks {
				if _, ok := rawHook.(map[string]any); !ok {
					return nil, true, fmt.Errorf("%s: hooks.%s[%d].hooks[%d] must be an object", path, event, index, hookIndex)
				}
			}
		}
	}
	return document, true, nil
}

func v2HooksMap(document map[string]any) map[string]any {
	hooks, ok := document["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		document["hooks"] = hooks
	}
	return hooks
}

func v2HookEventKeys(hooks map[string]any) []string {
	events := make([]string, 0, len(hooks))
	for event := range hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	return events
}

func v2EventGroups(hooks map[string]any, event string) []any {
	groups, _ := hooks[event].([]any)
	return groups
}

func v2SetEventGroups(hooks map[string]any, event string, groups []any) {
	if len(groups) == 0 {
		delete(hooks, event)
		return
	}
	hooks[event] = groups
}

func v2AppendHookGroup(hooks map[string]any, event, matcher string) {
	hook := map[string]any{
		"type":    "command",
		"command": v2HookCommand,
		"timeout": v2HookTimeout,
	}
	group := map[string]any{"hooks": []any{hook}}
	if matcher != "" {
		group["matcher"] = matcher
	}
	hooks[event] = append(v2EventGroups(hooks, event), group)
}

// v2FilterHookScripts removes hook commands that mention one of the named
// scripts. A group that loses all its hooks is dropped; other hooks in a
// shared group are preserved.
func v2FilterHookScripts(groups []any, names []string) ([]any, int) {
	keptGroups := make([]any, 0, len(groups))
	removed := 0
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			keptGroups = append(keptGroups, rawGroup)
			continue
		}
		rawHooks, ok := group["hooks"].([]any)
		if !ok {
			keptGroups = append(keptGroups, rawGroup)
			continue
		}
		keptHooks := make([]any, 0, len(rawHooks))
		for _, rawHook := range rawHooks {
			hook, ok := rawHook.(map[string]any)
			if !ok {
				keptHooks = append(keptHooks, rawHook)
				continue
			}
			command := asString(hook["command"])
			if v2CommandMentions(command, names) {
				removed++
				continue
			}
			keptHooks = append(keptHooks, rawHook)
		}
		if len(keptHooks) == 0 {
			continue
		}
		group["hooks"] = keptHooks
		keptGroups = append(keptGroups, group)
	}
	return keptGroups, removed
}

func v2CommandMentions(command string, names []string) bool {
	for _, name := range names {
		if strings.Contains(command, name) {
			return true
		}
	}
	return false
}

func v2HookScriptInstalled(hooks map[string]any, event string) bool {
	installed := false
	for _, rawGroup := range v2EventGroups(hooks, event) {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		rawHooks, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for _, rawHook := range rawHooks {
			if hook, ok := rawHook.(map[string]any); ok && v2CommandMentions(asString(hook["command"]), []string{v2HookScriptName}) {
				installed = true
			}
		}
	}
	return installed
}

func v2InstalledHookEvents(hooks map[string]any) []string {
	events := make([]string, 0)
	for _, event := range v2HookEventKeys(hooks) {
		if v2HookScriptInstalled(hooks, event) {
			events = append(events, event)
		}
	}
	return v2SortHookEvents(events)
}

func v2AnyHookScript(hooks map[string]any, name string) bool {
	for _, event := range v2HookEventKeys(hooks) {
		for _, rawGroup := range v2EventGroups(hooks, event) {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				continue
			}
			rawHooks, ok := group["hooks"].([]any)
			if !ok {
				continue
			}
			for _, rawHook := range rawHooks {
				if hook, ok := rawHook.(map[string]any); ok && v2CommandMentions(asString(hook["command"]), []string{name}) {
					return true
				}
			}
		}
	}
	return false
}

// v2WriteHooksDocument preserves the existing file mode so install does not
// silently tighten or loosen permissions on a user-owned config file.
func v2WriteHooksDocument(path string, document map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}
