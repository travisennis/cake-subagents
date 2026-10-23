// Package subagent implements the seven Cake toolbox tools that run and
// message background subagents.
package subagent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxRuntime   = 10_800
	maxBody      = 16_384
	maxLimit     = 100
	peerWait     = 30
	compactBytes = 38_000
	startupWait  = time.Second
)

var (
	uuidPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	depthPattern = regexp.MustCompile(`^[0-9]+$`)
	kinds        = []string{"note", "request", "reply", "event"}
	stringKeys   = []string{"prompt", "cwd", "sandbox", "type", "session_id", "run_id", "to", "body", "kind", "reply_to", "from", "name", "team"}
	uuidKeys     = []string{"run_id", "session_id", "reply_to"}
	booleanKeys  = []string{"peek", "all"}
	activeStates = map[string]bool{"starting": true, "running": true, "stopping": true}
)

var toolboxModes = map[string]string{
	"subagent_async":  "launch",
	"subagent_status": "status",
	"subagent_cancel": "cancel",
	"subagent_send":   "post",
	"subagent_inbox":  "inbox",
	"subagent_wait":   "wait",
	"subagent_peers":  "peers",
}

var toolNames = map[string]string{
	"launch": "subagent_async",
	"status": "subagent_status",
	"cancel": "subagent_cancel",
	"post":   "subagent_send",
	"inbox":  "subagent_inbox",
	"wait":   "subagent_wait",
	"peers":  "subagent_peers",
}

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

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
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

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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

func runDirectory(id string) (string, error) {
	directory := filepath.Join(runsRoot(), id)
	if info, err := os.Stat(filepath.Join(directory, "status.json")); err != nil || info.IsDir() {
		return "", fmt.Errorf("unknown run_id %s", id)
	}
	return directory, nil
}

func mailboxDir(directory string) string {
	return filepath.Join(directory, "mailbox")
}

func messagesDir(directory string) string {
	return filepath.Join(mailboxDir(directory), "msg")
}

func ensureMailbox(directory string) error {
	return os.MkdirAll(messagesDir(directory), 0o700)
}

func messageID() string {
	var random [4]byte
	_, _ = rand.Read(random[:])
	return fmt.Sprintf("%013d-%s", time.Now().UnixMilli(), hex.EncodeToString(random[:]))
}

func deliver(directory string, message map[string]any) error {
	if err := ensureMailbox(directory); err != nil {
		return err
	}
	return writeJSON(filepath.Join(messagesDir(directory), stringField(message, "id")+".json"), message)
}

func mailboxMessages(directory string) []map[string]any {
	entries, err := os.ReadDir(messagesDir(directory))
	if err != nil {
		return nil
	}
	messages := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if message := safeReadJSON(filepath.Join(messagesDir(directory), entry.Name())); message != nil {
			messages = append(messages, message)
		}
	}
	return messages
}

func readCursor(directory string) string {
	data, err := os.ReadFile(filepath.Join(mailboxDir(directory), "cursor"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeCursor(directory, value string) error {
	if err := os.MkdirAll(mailboxDir(directory), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(mailboxDir(directory), "cursor"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := flock(file, false); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	_, err = file.WriteString(value)
	return err
}

func unreadMessages(directory string) []map[string]any {
	cursor := readCursor(directory)
	unread := make([]map[string]any, 0)
	for _, message := range mailboxMessages(directory) {
		if stringField(message, "id") > cursor {
			unread = append(unread, message)
		}
	}
	return unread
}

// agentType is a configured subagent under cwd/.agents/subagents/<name>.md.
type agentType struct {
	Name    string
	Sandbox string
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
			if key != "name" && key != "sandbox" {
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
		result = append(result, agentType{Name: expected, Sandbox: sandbox, Body: strings.Join(lines[closing+1:], "")})
	}
	return result, nil
}

func rosterDir() string {
	return filepath.Join(runsRoot(), "roster")
}

func rosterWrite(entry map[string]any) error {
	if err := os.MkdirAll(rosterDir(), 0o700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(rosterDir(), stringField(entry, "run_id")+".json"), entry)
}

// rosterEntries returns static identity joined with live status, so a deleted
// run disappears and a finished one reports terminal state.
func rosterEntries() []map[string]any {
	entries, err := os.ReadDir(rosterDir())
	if err != nil {
		return nil
	}
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		roster := safeReadJSON(filepath.Join(rosterDir(), entry.Name()))
		if roster == nil || stringField(roster, "run_id") == "" {
			continue
		}
		live := safeReadJSON(filepath.Join(runsRoot(), stringField(roster, "run_id"), "status.json"))
		if live == nil {
			continue
		}
		roster["session_id"] = live["session_id"]
		roster["status"] = live["status"]
		result = append(result, roster)
	}
	return result
}

func runSessionID(id string) any {
	state := safeReadJSON(filepath.Join(runsRoot(), id, "status.json"))
	if state == nil {
		return nil
	}
	return state["session_id"]
}

func selfRunID() string {
	value := os.Getenv("CAKE_SUBAGENT_RUN_ID")
	if isUUID(value) {
		return strings.ToLower(value)
	}
	return ""
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
