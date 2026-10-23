package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

func allowedKeys(mode string) []string {
	switch mode {
	case "launch":
		return []string{"prompt", "cwd", "sandbox", "type", "session_id", "timeout_seconds", "name", "team"}
	case "status":
		return []string{"run_id", "wait_seconds"}
	case "cancel":
		return []string{"run_id"}
	case "post":
		return []string{"to", "body", "kind", "reply_to", "from"}
	case "inbox":
		return []string{"peek", "limit", "all"}
	case "wait":
		return []string{"timeout_seconds", "limit"}
	case "peers":
		return []string{"team"}
	}
	return nil
}

func requiredKeys(mode string) []string {
	switch mode {
	case "launch":
		return []string{"prompt"}
	case "status", "cancel":
		return []string{"run_id"}
	case "post":
		return []string{"to", "body"}
	}
	return nil
}

func integerRanges(mode string) map[string][2]int64 {
	maximum := int64(maxRuntime)
	if mode == "wait" {
		maximum = peerWait
	}
	return map[string][2]int64{
		"timeout_seconds": {1, maximum},
		"wait_seconds":    {0, 30},
		"limit":           {1, maxLimit},
	}
}

// decodeArguments reads the toolbox execute payload from stdin and rejects any
// argument the tool did not advertise. Cake only guarantees a JSON object.
func decodeArguments(mode string) (map[string]any, error) {
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
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	allowed := allowedKeys(mode)
	if allowed == nil {
		return nil, errors.New("unknown tool mode")
	}
	unsupported := make([]string, 0)
	for key := range value {
		if !contains(allowed, key) {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, fmt.Errorf("unsupported argument(s): %s", strings.Join(unsupported, ", "))
	}
	for _, key := range requiredKeys(mode) {
		if _, present := value[key]; !present {
			return nil, fmt.Errorf("%s is required", key)
		}
	}
	for _, key := range stringKeys {
		raw, present := value[key]
		if !present {
			continue
		}
		text, ok := raw.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("%s must be a non-empty string", key)
		}
		if strings.ContainsRune(text, 0) {
			return nil, fmt.Errorf("%s cannot contain NUL", key)
		}
	}
	for _, key := range uuidKeys {
		if !valueHas(value, key) {
			continue
		}
		text := stringField(value, key)
		if !isUUID(text) {
			return nil, fmt.Errorf("%s must be a UUID", key)
		}
		value[key] = strings.ToLower(text)
	}
	for _, key := range []string{"name", "team"} {
		if !valueHas(value, key) {
			continue
		}
		if !namePattern.MatchString(stringField(value, key)) {
			return nil, fmt.Errorf("%s must match %s", key, namePattern)
		}
	}
	if valueHas(value, "kind") && !contains(kinds, stringField(value, "kind")) {
		return nil, fmt.Errorf("kind must be one of %s", strings.Join(kinds, ", "))
	}
	if valueHas(value, "to") && strings.ContainsAny(stringField(value, "to"), " \t\r\n\f\v") {
		return nil, errors.New("to must not contain whitespace")
	}
	if valueHas(value, "body") && len(stringField(value, "body")) > maxBody {
		return nil, fmt.Errorf("body is too long (maximum %d bytes)", maxBody)
	}
	if valueHas(value, "sandbox") {
		sandbox := stringField(value, "sandbox")
		if sandbox != "read-only" && sandbox != "workspace-write" {
			return nil, errors.New("sandbox must be read-only or workspace-write")
		}
	}
	ranges := integerRanges(mode)
	for key, bounds := range ranges {
		raw, present := value[key]
		if !present {
			continue
		}
		number, ok := raw.(json.Number)
		if !ok {
			return nil, fmt.Errorf("%s must be an integer in %d..%d", key, bounds[0], bounds[1])
		}
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || parsed < bounds[0] || parsed > bounds[1] {
			return nil, fmt.Errorf("%s must be an integer in %d..%d", key, bounds[0], bounds[1])
		}
		value[key] = parsed
	}
	for _, key := range booleanKeys {
		if !valueHas(value, key) {
			continue
		}
		if _, ok := value[key].(bool); !ok {
			return nil, fmt.Errorf("%s must be a boolean", key)
		}
	}
	return value, nil
}

func valueHas(value map[string]any, key string) bool {
	_, present := value[key]
	return present
}

func intArg(args map[string]any, key string, fallback int64) int64 {
	if !valueHas(args, key) {
		return fallback
	}
	number, ok := asInt64(args[key])
	if !ok {
		return fallback
	}
	return number
}

func boolArg(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}
