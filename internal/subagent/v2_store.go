package subagent

import (
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
	"unicode"
)

const (
	v2MaxChildren     = 64
	v2MaxQueue        = 64
	v2DefaultWait     = 30
	v2MaxWait         = 300
	v2DefaultRuntime  = 10_800
	v2MaxInstructions = 64 * 1024
	v2InboxLimit      = 100
)

var (
	v2NamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	v2ReportValues  = []string{"final", "every_turn", "never"}
	v2EffortValues  = []string{"none", "low", "medium", "high", "xhigh", "max"}
	v2ANSI          = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")
	errV2QueueFull  = fmt.Errorf("queue_full: a child may have at most %d messages waiting", v2MaxQueue)
	errV2Capacity   = fmt.Errorf("capacity: a parent may have at most %d open children", v2MaxChildren)
	errV2NameParent = errors.New("agent name \"parent\" is reserved for child-to-parent messages")
)

type v2Child struct {
	ChildID       string `json:"child_id"`
	Name          string `json:"name"`
	OwnerID       string `json:"owner_id"`
	ParentAgent   string `json:"parent_agent,omitempty"`
	Type          string `json:"type"`
	Cwd           string `json:"cwd"`
	Sandbox       string `json:"sandbox"`
	Instructions  string `json:"instructions,omitempty"`
	SystemPrompt  string `json:"system_prompt,omitempty"`
	Model         string `json:"model,omitempty"`
	Profile       string `json:"profile,omitempty"`
	Effort        string `json:"effort,omitempty"`
	Report        string `json:"report"`
	SessionID     string `json:"session_id,omitempty"`
	State         string `json:"state"`
	LastOutcome   string `json:"last_outcome"`
	LastRunID     string `json:"last_run_id,omitempty"`
	SupervisorPID int    `json:"supervisor_pid,omitempty"`
	CreatedAt     string `json:"created_at"`
	LastActiveAt  string `json:"last_active_at"`
	ClosedAt      string `json:"closed_at,omitempty"`
}

type v2QueuedMessage struct {
	ID             string `json:"id"`
	ChildID        string `json:"child_id"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	CreatedAt      string `json:"created_at"`
}

type v2Caller struct {
	OwnerID   string
	ChildID   string
	Name      string
	RunID     string
	SessionID string
	Depth     int
}

func (caller v2Caller) isChild() bool {
	return caller.ChildID != "" || caller.Depth >= 1
}

func v2Root() string {
	return filepath.Join(runsRoot(), "v2")
}

func v2ChildrenRoot() string {
	return filepath.Join(v2Root(), "children")
}

func v2ChildDir(id string) (string, error) {
	if !isUUID(id) {
		return "", fmt.Errorf("invalid child_id %q", id)
	}
	return filepath.Join(v2ChildrenRoot(), strings.ToLower(id)), nil
}

func v2ParentDir(owner string) (string, error) {
	normalized, err := v2NormalizeOwner(owner)
	if err != nil {
		return "", err
	}
	return filepath.Join(v2Root(), "parents", normalized), nil
}

func v2NamesDir(owner string) (string, error) {
	directory, err := v2ParentDir(owner)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "names"), nil
}

func v2InboxDir(owner string) (string, error) {
	directory, err := v2ParentDir(owner)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "inbox"), nil
}

func v2NamePath(owner, agent string) (string, error) {
	if err := v2ValidateName(agent, false); err != nil {
		return "", err
	}
	directory, err := v2NamesDir(owner)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, agent), nil
}

func v2EnsureDir(path string) error {
	return os.MkdirAll(path, 0o700)
}

func v2WriteText(path, text string) error {
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(temporary, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func v2ReadText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func v2WithLock(path string, action func() error) error {
	if err := v2EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := flock(file, false); err != nil {
		return err
	}
	return action()
}

// v2NextID gives every durable queue and inbox entry a total order. The lock
// around the caller protects the scan, so two enqueues in the same millisecond
// cannot swap places.
func v2NextID(directory string) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var maximum int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		prefix := strings.SplitN(entry.Name(), "-", 2)[0]
		number, err := strconv.ParseInt(prefix, 10, 64)
		if err == nil && number > maximum {
			maximum = number
		}
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%020d-%s", maximum+1, hex.EncodeToString(random[:])), nil
}

func v2NormalizeOwner(owner string) (string, error) {
	if !strings.HasPrefix(owner, "session-") {
		return "", fmt.Errorf("invalid owner_id %q", owner)
	}
	id := strings.TrimPrefix(owner, "session-")
	if !isUUID(id) {
		return "", fmt.Errorf("invalid owner_id %q", owner)
	}
	return "session-" + strings.ToLower(id), nil
}

func v2OwnerFromThreadID(id string) (string, error) {
	if !isUUID(id) {
		return "", fmt.Errorf("CAKE_THREAD_ID must be a UUID, got %q", id)
	}
	return "session-" + strings.ToLower(id), nil
}

func v2ParentOwner() (string, error) {
	if value := os.Getenv("CAKE_THREAD_ID"); value != "" {
		return v2OwnerFromThreadID(value)
	}
	if value := os.Getenv("CAKE_SUBAGENT_PARENT_OWNER"); value != "" {
		return v2NormalizeOwner(value)
	}
	return "", errors.New("CAKE_THREAD_ID is required to identify the parent session")
}

func v2CallerIdentity() (v2Caller, error) {
	var caller v2Caller
	if text := os.Getenv("CAKE_SUBAGENT_DEPTH"); text != "" {
		if !depthPattern.MatchString(text) {
			return caller, errors.New("invalid CAKE_SUBAGENT_DEPTH")
		}
		value, err := strconv.Atoi(text)
		if err != nil {
			return caller, errors.New("invalid CAKE_SUBAGENT_DEPTH")
		}
		caller.Depth = value
	}
	childID := strings.ToLower(os.Getenv("CAKE_SUBAGENT_CHILD_ID"))
	if childID != "" {
		if !isUUID(childID) {
			return caller, fmt.Errorf("invalid CAKE_SUBAGENT_CHILD_ID %q", childID)
		}
		caller.ChildID = childID
	}
	if caller.isChild() {
		owner, err := v2NormalizeOwner(os.Getenv("CAKE_SUBAGENT_PARENT_OWNER"))
		if err != nil {
			return caller, err
		}
		caller.OwnerID = owner
		caller.Name = os.Getenv("CAKE_SUBAGENT_NAME")
		if caller.Name != "" {
			if err := v2ValidateName(caller.Name, true); err != nil {
				return caller, err
			}
		}
		caller.RunID = strings.ToLower(os.Getenv("CAKE_SUBAGENT_RUN_ID"))
		if caller.RunID != "" && !isUUID(caller.RunID) {
			return caller, fmt.Errorf("invalid CAKE_SUBAGENT_RUN_ID %q", caller.RunID)
		}
		caller.SessionID = strings.ToLower(os.Getenv("CAKE_SUBAGENT_SESSION_ID"))
		if caller.SessionID != "" && !isUUID(caller.SessionID) {
			return caller, fmt.Errorf("invalid CAKE_SUBAGENT_SESSION_ID %q", caller.SessionID)
		}
		return caller, nil
	}
	owner, err := v2ParentOwner()
	if err != nil {
		return caller, err
	}
	caller.OwnerID = owner
	return caller, nil
}

func v2ValidateName(name string, allowParent bool) error {
	if name == "parent" {
		if allowParent {
			return nil
		}
		return errV2NameParent
	}
	if !v2NamePattern.MatchString(name) {
		return fmt.Errorf("agent must match %s", v2NamePattern)
	}
	return nil
}

func v2ReadChild(id string) (*v2Child, error) {
	directory, err := v2ChildDir(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(directory, "child.json"))
	if err != nil {
		return nil, err
	}
	var child v2Child
	if err := json.Unmarshal(data, &child); err != nil {
		return nil, err
	}
	if child.ChildID == "" || strings.ToLower(child.ChildID) != strings.ToLower(id) {
		return nil, fmt.Errorf("child record does not match child_id %s", id)
	}
	if child.Report == "" {
		child.Report = "final"
	}
	if child.LastOutcome == "" {
		child.LastOutcome = "never"
	}
	return &child, nil
}

func v2WriteChild(child *v2Child) error {
	if child.Report == "" {
		child.Report = "final"
	}
	if child.LastOutcome == "" {
		child.LastOutcome = "never"
	}
	directory, err := v2ChildDir(child.ChildID)
	if err != nil {
		return err
	}
	if err := v2EnsureDir(directory); err != nil {
		return err
	}
	return writeJSON(filepath.Join(directory, "child.json"), child)
}

func v2UpdateChild(id string, mutate func(*v2Child) error) (*v2Child, error) {
	directory, err := v2ChildDir(id)
	if err != nil {
		return nil, err
	}
	var updated *v2Child
	err = v2WithLock(filepath.Join(directory, "child.lock"), func() error {
		child, err := v2ReadChild(id)
		if err != nil {
			return err
		}
		if err := mutate(child); err != nil {
			return err
		}
		child.LastActiveAt = now()
		if err := v2WriteChild(child); err != nil {
			return err
		}
		updated = child
		return nil
	})
	return updated, err
}

func v2ListChildren(owner string) ([]*v2Child, error) {
	entries, err := os.ReadDir(v2ChildrenRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	normalized, err := v2NormalizeOwner(owner)
	if err != nil {
		return nil, err
	}
	children := make([]*v2Child, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !isUUID(entry.Name()) {
			continue
		}
		child, err := v2ReadChild(entry.Name())
		if err != nil {
			continue
		}
		if child.OwnerID != normalized || child.State == "closed" {
			continue
		}
		children = append(children, child)
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	return children, nil
}

func v2ResolveName(owner, agent string) (string, error) {
	path, err := v2NamePath(owner, agent)
	if err != nil {
		return "", err
	}
	id, err := v2ReadText(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !isUUID(id) {
		return "", fmt.Errorf("%s: name mapping is not a UUID", path)
	}
	return strings.ToLower(id), nil
}

func v2WriteNameUnlocked(owner, agent, childID string) error {
	path, err := v2NamePath(owner, agent)
	if err != nil {
		return err
	}
	if err := v2EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return v2WriteText(path, childID+"\n")
}

func v2RemoveNameUnlocked(owner, agent, childID string) error {
	path, err := v2NamePath(owner, agent)
	if err != nil {
		return err
	}
	current, err := v2ReadText(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != "" && !strings.EqualFold(current, childID) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func v2RemoveName(owner, agent, childID string) error {
	parent, err := v2ParentDir(owner)
	if err != nil {
		return err
	}
	return v2WithLock(filepath.Join(parent, "names.lock"), func() error {
		return v2RemoveNameUnlocked(owner, agent, childID)
	})
}

func v2EnsureChild(owner, agent string, spec v2CreateSpec) (*v2Child, bool, error) {
	parent, err := v2ParentDir(owner)
	if err != nil {
		return nil, false, err
	}
	var child *v2Child
	created := false
	err = v2WithLock(filepath.Join(parent, "names.lock"), func() error {
		current, err := v2ResolveName(owner, agent)
		if err != nil {
			return err
		}
		if current != "" {
			existing, err := v2ReadChild(current)
			if err != nil {
				return err
			}
			if existing.State == "closed" {
				if err := v2RemoveNameUnlocked(owner, agent, current); err != nil {
					return err
				}
			} else if existing.State == "stopping" {
				return fmt.Errorf("agent %q is closing", agent)
			} else if existing.State == "unknown" {
				return fmt.Errorf("agent %q is unknown; inspect its last run before sending more work", agent)
			} else {
				child = existing
				return nil
			}
		}
		children, err := v2ListChildren(owner)
		if err != nil {
			return err
		}
		if len(children) >= v2MaxChildren {
			return errV2Capacity
		}
		id, err := newUUID()
		if err != nil {
			return err
		}
		directory, err := v2ChildDir(id)
		if err != nil {
			return err
		}
		for _, name := range []string{directory, filepath.Join(directory, "queue"), filepath.Join(directory, "runs")} {
			if err := v2EnsureDir(name); err != nil {
				return err
			}
		}
		child = &v2Child{
			ChildID: id, Name: agent, OwnerID: owner, Type: spec.Type, Cwd: spec.Cwd,
			Sandbox: spec.Sandbox, Instructions: spec.Instructions, SystemPrompt: spec.SystemPrompt,
			Model: spec.Model, Profile: spec.Profile, Effort: spec.Effort, Report: spec.Report,
			SessionID: spec.SessionID, State: "starting", LastOutcome: "never",
			CreatedAt: now(), LastActiveAt: now(),
		}
		if err := v2WriteChild(child); err != nil {
			return err
		}
		if err := v2WriteNameUnlocked(owner, agent, id); err != nil {
			return err
		}
		created = true
		return nil
	})
	return child, created, err
}

func v2Enqueue(childID, prompt string, timeout int) (*v2QueuedMessage, error) {
	directory, err := v2ChildDir(childID)
	if err != nil {
		return nil, err
	}
	queue := filepath.Join(directory, "queue")
	if err := v2EnsureDir(queue); err != nil {
		return nil, err
	}
	var message *v2QueuedMessage
	err = v2WithLock(filepath.Join(directory, "queue.lock"), func() error {
		count, err := v2QueueCountUnlocked(queue)
		if err != nil {
			return err
		}
		if count >= v2MaxQueue {
			return errV2QueueFull
		}
		id, err := v2NextID(queue)
		if err != nil {
			return err
		}
		message = &v2QueuedMessage{
			ID: id, ChildID: childID, Prompt: prompt, TimeoutSeconds: timeout, CreatedAt: now(),
		}
		return writeJSON(filepath.Join(queue, id+".json"), message)
	})
	return message, err
}

func v2QueueCountUnlocked(queue string) (int, error) {
	entries, err := os.ReadDir(queue)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			count++
		}
	}
	return count, nil
}

func v2QueueCount(childID string) (int, error) {
	directory, err := v2ChildDir(childID)
	if err != nil {
		return 0, err
	}
	queue := filepath.Join(directory, "queue")
	var count int
	err = v2WithLock(filepath.Join(directory, "queue.lock"), func() error {
		var err error
		count, err = v2QueueCountUnlocked(queue)
		return err
	})
	return count, err
}

func v2Dequeue(childID string) (*v2QueuedMessage, error) {
	directory, err := v2ChildDir(childID)
	if err != nil {
		return nil, err
	}
	queue := filepath.Join(directory, "queue")
	var message *v2QueuedMessage
	err = v2WithLock(filepath.Join(directory, "queue.lock"), func() error {
		entries, err := os.ReadDir(queue)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(queue, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var queued v2QueuedMessage
			if err := json.Unmarshal(data, &queued); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			message = &queued
			return nil
		}
		return nil
	})
	return message, err
}

func v2DropQueue(childID string) (int, error) {
	directory, err := v2ChildDir(childID)
	if err != nil {
		return 0, err
	}
	queue := filepath.Join(directory, "queue")
	var dropped int
	err = v2WithLock(filepath.Join(directory, "queue.lock"), func() error {
		entries, err := os.ReadDir(queue)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			if err := os.Remove(filepath.Join(queue, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			dropped++
		}
		return nil
	})
	return dropped, err
}

func v2AppendInbox(owner string, item map[string]any) (map[string]any, error) {
	directory, err := v2InboxDir(owner)
	if err != nil {
		return nil, err
	}
	parent, err := v2ParentDir(owner)
	if err != nil {
		return nil, err
	}
	var stored map[string]any
	err = v2WithLock(filepath.Join(parent, "inbox.lock"), func() error {
		if err := v2EnsureDir(directory); err != nil {
			return err
		}
		id, err := v2NextID(directory)
		if err != nil {
			return err
		}
		item["id"] = id
		if stringField(item, "created_at") == "" {
			item["created_at"] = now()
		}
		stored = item
		return writeJSON(filepath.Join(directory, id+".json"), item)
	})
	return stored, err
}

func v2ReadCursor(owner string) string {
	directory, err := v2ParentDir(owner)
	if err != nil {
		return ""
	}
	cursor, err := v2ReadText(filepath.Join(directory, "cursor"))
	if err != nil {
		return ""
	}
	return cursor
}

func v2WriteCursorUnlocked(owner, cursor string) error {
	directory, err := v2ParentDir(owner)
	if err != nil {
		return err
	}
	if err := v2EnsureDir(directory); err != nil {
		return err
	}
	return v2WriteText(filepath.Join(directory, "cursor"), cursor+"\n")
}

func v2InboxItems(owner string) []map[string]any {
	directory, err := v2InboxDir(owner)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		item := safeReadJSON(filepath.Join(directory, entry.Name()))
		if item != nil {
			items = append(items, item)
		}
	}
	return items
}

func v2UnreadInbox(owner string) []map[string]any {
	cursor := v2ReadCursor(owner)
	unread := make([]map[string]any, 0)
	for _, item := range v2InboxItems(owner) {
		if stringField(item, "id") > cursor {
			unread = append(unread, item)
		}
	}
	return unread
}

// v2DrainInbox advances the cursor exactly once for the items it returns.
// With an agent filter it consumes only a contiguous matching prefix; a
// non-matching item at the front is left untouched rather than silently
// dropped.
func v2DrainInbox(owner, agent string, limit int) ([]map[string]any, int) {
	if limit <= 0 || limit > v2InboxLimit {
		limit = v2InboxLimit
	}
	parent, err := v2ParentDir(owner)
	if err != nil {
		return nil, 0
	}
	var selected []map[string]any
	unreadCount := 0
	_ = v2WithLock(filepath.Join(parent, "inbox.lock"), func() error {
		unread := v2UnreadInbox(owner)
		unreadCount = len(unread)
		if len(unread) == 0 {
			return nil
		}
		if agent != "" && stringField(unread[0], "agent") != agent {
			return nil
		}
		for _, item := range unread {
			if agent != "" && stringField(item, "agent") != agent {
				break
			}
			selected = append(selected, item)
			if len(selected) >= limit {
				break
			}
		}
		if len(selected) > 0 {
			if err := v2WriteCursorUnlocked(owner, stringField(selected[len(selected)-1], "id")); err != nil {
				return err
			}
		}
		return nil
	})
	sanitized := make([]map[string]any, 0, len(selected))
	for _, item := range selected {
		sanitized = append(sanitized, v2SanitizeItem(item))
	}
	return sanitized, unreadCount
}

func v2SanitizeItem(item map[string]any) map[string]any {
	result := make(map[string]any, len(item)+1)
	for key, value := range item {
		result[key] = value
	}
	for _, key := range []string{"text", "body", "error", "result"} {
		text, ok := result[key].(string)
		if !ok {
			continue
		}
		clean := v2StripControl(text)
		clean, truncated := v2BoundText(clean, compactBytes)
		result[key] = clean
		if truncated {
			result[key+"_truncated"] = true
		}
	}
	result["untrusted"] = true
	return result
}

func v2StripControl(text string) string {
	text = v2ANSI.ReplaceAllString(text, "")
	var builder strings.Builder
	builder.Grow(len(text))
	for _, character := range text {
		if character == '\n' || character == '\r' || character == '\t' {
			builder.WriteRune(character)
			continue
		}
		if unicode.IsControl(character) {
			continue
		}
		if (character >= 0x202a && character <= 0x202e) || (character >= 0x2066 && character <= 0x2069) {
			continue
		}
		builder.WriteRune(character)
	}
	return builder.String()
}

func v2BoundText(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if len(string(runes[:middle])) <= limit {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return string(runes[:low]), true
}

func v2HasOpenChildren(owner string) bool {
	children, err := v2ListChildren(owner)
	return err == nil && len(children) > 0
}

func v2SessionLock(id string) (*os.File, error) {
	if id == "" {
		return nil, nil
	}
	if !isUUID(id) {
		return nil, fmt.Errorf("invalid session_id %q", id)
	}
	directory := filepath.Join(v2Root(), "locks")
	if err := v2EnsureDir(directory); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, strings.ToLower(id)+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock(file, true); err != nil {
		file.Close()
		return nil, fmt.Errorf("session %s already has an active asynchronous run", id)
	}
	return file, nil
}
