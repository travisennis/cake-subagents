package subagent

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxBody      = 16_384
	compactBytes = 38_000
)

var (
	uuidPattern       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	depthPattern      = regexp.MustCompile(`^[0-9]+$`)
	configNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)
)

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func isUUID(value string) bool {
	return uuidPattern.MatchString(value)
}

func newUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[0:4], data[4:6], data[6:8], data[8:10], data[10:16]), nil
}

// marshalJSON matches Ruby's JSON.generate: compact output, no HTML escaping,
// one trailing newline.
func marshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeJSON(path string, value any) error {
	data, err := marshalJSON(value)
	if err != nil {
		return err
	}
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func readJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("%s: expected a JSON object", path)
	}
	return value, nil
}

func safeReadJSON(path string) map[string]any {
	value, err := readJSON(path)
	if err != nil {
		return nil
	}
	return value
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}

func asInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		return int64(number), number == float64(int64(number))
	case json.Number:
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		return parsed, err == nil
	case int:
		return int64(number), true
	case int64:
		return number, true
	}
	return 0, false
}

func stringField(value map[string]any, key string) string {
	return asString(value[key])
}

func intField(value map[string]any, key string) int64 {
	number, _ := asInt64(value[key])
	return number
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

func valueHas(value map[string]any, key string) bool {
	_, present := value[key]
	return present
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func expandFrom(path, base string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

func dataDir() string {
	if value := os.Getenv("CAKE_DATA_DIR"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".local/share/cake"
	}
	return filepath.Join(home, ".local/share/cake")
}

func runsRoot() string {
	if value := os.Getenv("CAKE_SUBAGENT_RUNS_DIR"); value != "" {
		return expandFrom(value, ".")
	}
	return expandFrom(filepath.Join(dataDir(), "subagent-runs"), ".")
}

func sessionFile(id, cwd string) *string {
	if id == "" {
		return nil
	}
	path := filepath.Join(expandFrom(dataDir(), cwd), "sessions", id+".jsonl")
	return &path
}

// agentType is a configured subagent under cwd/.agents/subagents/<name>.md.
type agentType struct {
	Name    string
	Sandbox string
	Model   string
	Profile string
	Body    string
}

// splitLinesKeepEnds mirrors Ruby's String#lines: separators stay attached.
func splitLinesKeepEnds(text string) []string {
	lines := make([]string, 0)
	for len(text) > 0 {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:index+1])
		text = text[index+1:]
	}
	return lines
}

func configs(cwd string) ([]agentType, error) {
	directory := filepath.Join(cwd, ".agents", "subagents")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := make([]agentType, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		lines := splitLinesKeepEnds(string(data))
		if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
			return nil, fmt.Errorf("%s: YAML front matter must start with ---", path)
		}
		closing := -1
		for index := 1; index < len(lines); index++ {
			if strings.TrimRight(lines[index], "\r\n") == "---" {
				closing = index
				break
			}
		}
		if closing < 0 {
			return nil, fmt.Errorf("%s: YAML front matter has no closing ---", path)
		}
		var header map[string]any
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "")), &header); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if header == nil {
			return nil, fmt.Errorf("%s: YAML front matter must be a mapping", path)
		}
		for key := range header {
			if key != "name" && key != "sandbox" && key != "model" && key != "profile" {
				return nil, fmt.Errorf("%s: unsupported front-matter fields", path)
			}
		}
		expected := strings.TrimSuffix(entry.Name(), ".md")
		name, ok := header["name"].(string)
		if !ok || expected == "" || name != expected || expected == "general" {
			return nil, fmt.Errorf("%s: name must match filename and cannot be general", path)
		}
		sandbox := "workspace-write"
		if value, present := header["sandbox"]; present {
			sandbox, ok = value.(string)
			if !ok {
				return nil, fmt.Errorf("%s: invalid sandbox", path)
			}
		}
		if sandbox != "read-only" && sandbox != "workspace-write" && sandbox != "danger-full-access" {
			return nil, fmt.Errorf("%s: invalid sandbox", path)
		}
		model := ""
		if value, present := header["model"]; present {
			model, ok = value.(string)
			if !ok || !configNamePattern.MatchString(model) {
				return nil, fmt.Errorf("%s: invalid model", path)
			}
		}
		profile := ""
		if value, present := header["profile"]; present {
			profile, ok = value.(string)
			if !ok || !configNamePattern.MatchString(profile) {
				return nil, fmt.Errorf("%s: invalid profile", path)
			}
		}
		result = append(result, agentType{
			Name: expected, Sandbox: sandbox, Model: model, Profile: profile,
			Body: strings.Join(lines[closing+1:], ""),
		})
	}
	return result, nil
}

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

// execFailure matches Ruby's Errno::ENOENT message shape, which the offline
// suite asserts on for a missing CAKE_BIN.
func execFailure(err error, path string) error {
	if errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("No such file or directory - %s", path)
	}
	return err
}

// logTail returns the end of a log with whitespace collapsed and its length
// bounded, so a startup failure can name its cause in status instead of only
// in a file the caller has to open.
func logTail(path string, limit int) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	const readLimit = 1024
	offset := info.Size() - readLimit
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil {
		return ""
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	if len(text) > limit {
		text = text[:limit] + "..."
	}
	return text
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
