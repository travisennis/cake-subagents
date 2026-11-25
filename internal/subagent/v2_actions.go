package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type v2CreateSpec struct {
	Cwd          string
	Type         string
	Sandbox      string
	Instructions string
	SystemPrompt string
	Model        string
	Profile      string
	Effort       string
	Report       string
	SessionID    string
}

var v2CreationKeys = []string{"instructions", "model", "effort", "profile", "cwd", "sandbox", "type", "session_id"}

func v2AllowedKeys(action string) []string {
	switch action {
	case "message":
		return []string{"action", "agent", "message", "instructions", "report", "model", "effort", "profile", "cwd", "sandbox", "type", "session_id", "timeout_seconds"}
	case "wait":
		return []string{"action", "agent", "timeout_seconds"}
	case "status":
		return []string{"action", "agent"}
	case "stop", "close":
		return []string{"action", "agent"}
	}
	return nil
}

func v2RequiredKeys(action string) []string {
	switch action {
	case "message":
		return []string{"agent", "message"}
	case "stop", "close":
		return []string{"agent"}
	}
	return nil
}

func v2ValidateRequest(action string, args map[string]any) error {
	allowed := v2AllowedKeys(action)
	if allowed == nil {
		return fmt.Errorf("unknown v2 action %q", action)
	}
	unsupported := make([]string, 0)
	for key := range args {
		if !contains(allowed, key) {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return fmt.Errorf("unsupported argument(s): %s", strings.Join(unsupported, ", "))
	}
	for _, key := range v2RequiredKeys(action) {
		if !valueHas(args, key) {
			return fmt.Errorf("%s is required", key)
		}
	}
	for key, raw := range args {
		if key == "timeout_seconds" {
			continue
		}
		text, ok := raw.(string)
		if !ok || text == "" {
			return fmt.Errorf("%s must be a non-empty string", key)
		}
		if strings.ContainsRune(text, 0) {
			return fmt.Errorf("%s cannot contain NUL", key)
		}
	}
	if valueHas(args, "agent") {
		if err := v2ValidateName(stringField(args, "agent"), action == "message"); err != nil {
			return err
		}
	}
	if valueHas(args, "report") && !contains(v2ReportValues, stringField(args, "report")) {
		return fmt.Errorf("report must be one of %s", strings.Join(v2ReportValues, ", "))
	}
	if valueHas(args, "effort") && !contains(v2EffortValues, stringField(args, "effort")) {
		return fmt.Errorf("effort must be one of %s", strings.Join(v2EffortValues, ", "))
	}
	if valueHas(args, "sandbox") {
		sandbox := stringField(args, "sandbox")
		if sandbox != "read-only" && sandbox != "workspace-write" {
			return errors.New("sandbox must be read-only or workspace-write")
		}
	}
	if valueHas(args, "model") && !configNamePattern.MatchString(stringField(args, "model")) {
		return fmt.Errorf("invalid model %q", stringField(args, "model"))
	}
	if valueHas(args, "profile") && !configNamePattern.MatchString(stringField(args, "profile")) {
		return fmt.Errorf("invalid profile %q", stringField(args, "profile"))
	}
	if valueHas(args, "type") && !configNamePattern.MatchString(stringField(args, "type")) {
		return fmt.Errorf("invalid type %q", stringField(args, "type"))
	}
	if valueHas(args, "session_id") {
		value := stringField(args, "session_id")
		if !isUUID(value) {
			return errors.New("session_id must be a UUID")
		}
		args["session_id"] = strings.ToLower(value)
	}
	if valueHas(args, "message") && len(stringField(args, "message")) > maxBody {
		return fmt.Errorf("message is too long (maximum %d bytes)", maxBody)
	}
	if valueHas(args, "instructions") && len(stringField(args, "instructions")) > v2MaxInstructions {
		return fmt.Errorf("instructions are too long (maximum %d bytes)", v2MaxInstructions)
	}
	if valueHas(args, "timeout_seconds") {
		number, ok := args["timeout_seconds"].(json.Number)
		if !ok {
			return errors.New("timeout_seconds must be an integer")
		}
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil {
			return errors.New("timeout_seconds must be an integer")
		}
		minimum, maximum := int64(1), int64(v2DefaultRuntime)
		if action == "wait" {
			minimum, maximum = 0, v2MaxWait
		}
		if parsed < minimum || parsed > maximum {
			return fmt.Errorf("timeout_seconds must be an integer in %d..%d", minimum, maximum)
		}
		args["timeout_seconds"] = parsed
	}
	return nil
}

func v2BuildSpec(args map[string]any) (v2CreateSpec, error) {
	var spec v2CreateSpec
	cwdArg := stringField(args, "cwd")
	if cwdArg == "" {
		var err error
		cwdArg, err = os.Getwd()
		if err != nil {
			return spec, err
		}
	}
	cwd, err := filepath.EvalSymlinks(cwdArg)
	if err != nil {
		return spec, err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return spec, err
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return spec, errors.New("cwd must be a directory")
	}
	spec.Cwd = cwd

	spec.Type = "general"
	if value := stringField(args, "type"); value != "" {
		spec.Type = value
	}
	var config *agentType
	if spec.Type != "general" {
		items, err := configs(cwd)
		if err != nil {
			return spec, err
		}
		for index := range items {
			if items[index].Name == spec.Type {
				config = &items[index]
				break
			}
		}
		if config == nil {
			return spec, fmt.Errorf("unknown subagent type %q", spec.Type)
		}
	}

	spec.Sandbox = "workspace-write"
	if value := stringField(args, "sandbox"); value != "" {
		spec.Sandbox = value
	}
	if config != nil {
		spec.Sandbox = config.Sandbox
	}

	// The operator environment wins over every launch so one install can
	// redirect all children. The explicit creation value is next, then the
	// configured type. Empty means Cake resolves this installation's own
	// default, so no host-specific entry is baked into the child record.
	spec.Model = strings.TrimSpace(os.Getenv("CAKE_SUBAGENT_MODEL"))
	if spec.Model == "" {
		spec.Model = strings.TrimSpace(stringField(args, "model"))
	}
	spec.Profile = strings.TrimSpace(os.Getenv("CAKE_SUBAGENT_PROFILE"))
	if spec.Profile == "" {
		spec.Profile = strings.TrimSpace(stringField(args, "profile"))
	}
	if config != nil {
		if spec.Model == "" {
			spec.Model = config.Model
		}
		if spec.Profile == "" {
			spec.Profile = config.Profile
		}
	}
	if spec.Model != "" && !configNamePattern.MatchString(spec.Model) {
		return spec, fmt.Errorf("invalid model %q", spec.Model)
	}
	if spec.Profile != "" && !configNamePattern.MatchString(spec.Profile) {
		return spec, fmt.Errorf("invalid profile %q", spec.Profile)
	}
	spec.Effort = stringField(args, "effort")

	spec.Report = "final"
	if value := stringField(args, "report"); value != "" {
		spec.Report = value
	}
	spec.Instructions = stringField(args, "instructions")
	if config != nil {
		spec.SystemPrompt = config.Body
	}
	if spec.Instructions != "" {
		if spec.SystemPrompt != "" && !strings.HasSuffix(spec.SystemPrompt, "\n") {
			spec.SystemPrompt += "\n"
		}
		if spec.SystemPrompt != "" {
			spec.SystemPrompt += "\n"
		}
		spec.SystemPrompt += spec.Instructions
	}
	if value := stringField(args, "session_id"); value != "" {
		spec.SessionID = strings.ToLower(value)
	}
	return spec, nil
}

func v2Message(caller v2Caller, args map[string]any) (map[string]any, error) {
	agent := stringField(args, "agent")
	message := stringField(args, "message")
	if caller.isChild() {
		if agent != "parent" {
			return nil, errors.New("nested delegation is not allowed")
		}
		item := map[string]any{
			"kind": "message", "agent": caller.Name, "child_id": caller.ChildID,
			"run_id": caller.RunID, "session_id": caller.SessionID,
			"body": message, "text": message, "created_at": now(),
		}
		stored, err := v2AppendInbox(caller.OwnerID, item)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"to": "parent", "message_id": stringField(stored, "id")}
		if caller.ChildID != "" {
			result["child_id"] = caller.ChildID
		}
		if caller.Name != "" {
			result["agent"] = caller.Name
		}
		return result, nil
	}
	if agent == "parent" {
		return nil, errV2NameParent
	}

	existingID, err := v2ResolveName(caller.OwnerID, agent)
	if err != nil {
		return nil, err
	}
	if existingID != "" {
		child, err := v2ReadChild(existingID)
		if err != nil {
			return nil, err
		}
		if v2ChildUnknown(child, v2SupervisorLive(child.ChildID)) {
			v2MaybeReportUnknown(child, v2UnknownError)
			return nil, fmt.Errorf("agent %q is unknown; inspect its last run before sending more work", agent)
		}
		if child.State == "closed" {
			if err := v2RemoveName(caller.OwnerID, agent, existingID); err != nil {
				return nil, err
			}
			return v2CreateAndMessage(caller, agent, args)
		}
		if child.State == "stopping" {
			return nil, fmt.Errorf("agent %q is closing", agent)
		}
		if child.State == "unknown" {
			return nil, fmt.Errorf("agent %q is unknown; inspect its last run before sending more work", agent)
		}
		for _, key := range v2CreationKeys {
			if valueHas(args, key) {
				return nil, fmt.Errorf("%s is creation-only; close the child and create a new one", key)
			}
		}
		queued, err := v2Enqueue(child.ChildID, message, int(intArg(args, "timeout_seconds", v2DefaultRuntime)))
		if err != nil {
			return nil, err
		}
		if valueHas(args, "report") {
			child, err = v2UpdateChild(child.ChildID, func(record *v2Child) error {
				record.Report = stringField(args, "report")
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		spawned, err := v2SpawnSupervisor(child.ChildID, nil)
		if err != nil {
			return nil, err
		}
		return v2MessageResult(child, true, spawned, queued)
	}
	return v2CreateAndMessage(caller, agent, args)
}

func v2CreateAndMessage(caller v2Caller, agent string, args map[string]any) (map[string]any, error) {
	spec, err := v2BuildSpec(args)
	if err != nil {
		return nil, err
	}
	var sessionLock *os.File
	if spec.SessionID != "" {
		sessionLock, err = v2SessionLock(spec.SessionID)
		if err != nil {
			return nil, err
		}
		defer func() {
			if sessionLock != nil {
				sessionLock.Close()
			}
		}()
	}
	child, created, err := v2EnsureChild(caller.OwnerID, agent, spec)
	if err != nil {
		return nil, err
	}
	if !created {
		if sessionLock != nil {
			sessionLock.Close()
			sessionLock = nil
		}
		if v2ChildUnknown(child, v2SupervisorLive(child.ChildID)) {
			v2MaybeReportUnknown(child, v2UnknownError)
			return nil, fmt.Errorf("agent %q is unknown; inspect its last run before sending more work", agent)
		}
		if child.State == "stopping" {
			return nil, fmt.Errorf("agent %q is closing", agent)
		}
		for _, key := range v2CreationKeys {
			if valueHas(args, key) {
				return nil, fmt.Errorf("%s is creation-only; close the child and create a new one", key)
			}
		}
	}
	queued, err := v2Enqueue(child.ChildID, stringField(args, "message"), int(intArg(args, "timeout_seconds", v2DefaultRuntime)))
	if err != nil {
		return nil, err
	}
	spawned, err := v2SpawnSupervisor(child.ChildID, sessionLock)
	if err != nil {
		return nil, err
	}
	if sessionLock != nil {
		sessionLock.Close()
		sessionLock = nil
	}
	return v2MessageResult(child, created, spawned, queued)
}

func v2MessageResult(child *v2Child, created, spawned bool, queued *v2QueuedMessage) (map[string]any, error) {
	queuedCount, _ := v2QueueCount(child.ChildID)
	result := map[string]any{
		"agent": child.Name, "child_id": child.ChildID, "state": child.State,
		"created": created, "spawned": spawned, "queued": queuedCount,
	}
	if queued != nil {
		result["message_id"] = queued.ID
	}
	if child.SessionID != "" {
		result["session_id"] = child.SessionID
	}
	if child.Report != "" {
		result["report"] = child.Report
	}
	return result, nil
}

func v2Status(caller v2Caller, args map[string]any) (map[string]any, error) {
	if caller.isChild() {
		child, err := v2ReadChild(caller.ChildID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"self": v2ChildStatus(child), "children": []any{}, "reports": []any{}}, nil
	}
	agent := stringField(args, "agent")
	if agent != "" {
		id, err := v2ResolveName(caller.OwnerID, agent)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, fmt.Errorf("unknown agent %q", agent)
		}
		child, err := v2ReadChild(id)
		if err != nil {
			return nil, err
		}
		entry := v2ChildStatus(child)
		reports, _ := v2DrainInbox(caller.OwnerID, agent, v2InboxLimit)
		return map[string]any{"agent": agent, "children": []any{entry}, "reports": reports}, nil
	}
	children, err := v2ListChildren(caller.OwnerID)
	if err != nil {
		return nil, err
	}
	entries := make([]map[string]any, 0, len(children))
	for _, child := range children {
		entries = append(entries, v2ChildStatus(child))
	}
	// Run the child sweep before draining so a newly materialized unknown
	// report is visible in this status response.
	reports, _ := v2DrainInbox(caller.OwnerID, "", v2InboxLimit)
	return map[string]any{"children": entries, "reports": reports}, nil
}

func v2ChildStatus(child *v2Child) map[string]any {
	state := child.State
	errorText := ""
	live := v2SupervisorLive(child.ChildID)
	if v2ChildUnknown(child, live) {
		state = "unknown"
		errorText = v2UnknownError
		v2MaybeReportUnknown(child, errorText)
	}
	queued, _ := v2QueueCount(child.ChildID)
	entry := map[string]any{
		"agent": child.Name, "child_id": child.ChildID, "state": state,
		"last_outcome": child.LastOutcome, "queued": queued, "report": child.Report,
		"type": child.Type, "cwd": child.Cwd, "sandbox": child.Sandbox,
		"model": child.Model, "profile": child.Profile, "effort": child.Effort,
		"created_at": child.CreatedAt, "last_active_at": child.LastActiveAt,
	}
	if child.SessionID != "" {
		entry["session_id"] = child.SessionID
	}
	if child.LastRunID != "" {
		entry["last_run_id"] = child.LastRunID
		if directory, err := v2ChildDir(child.ChildID); err == nil {
			if run := safeReadJSON(filepath.Join(directory, "runs", child.LastRunID, "status.json")); run != nil {
				entry["last_run"] = v2SanitizeItem(compact(run))
			}
		}
	}
	if child.SupervisorPID != 0 {
		entry["supervisor_pid"] = child.SupervisorPID
	}
	if errorText != "" {
		entry["error"] = errorText
	}
	return entry
}

const v2UnknownError = "Supervisor exited without recording completion; the child's Cake process may still be running. Inspect the run and process before sending more work."

func v2ChildUnknown(child *v2Child, live bool) bool {
	if live {
		return false
	}
	switch child.State {
	case "running", "stopping", "unknown":
		return true
	case "starting":
		return v2StartingExpired(child)
	}
	return false
}

// v2MaterializeUnknown makes the "lost/unknown always reports" policy visible
// to wait and to any action that drains the inbox. The marker in
// v2MaybeReportUnknown keeps repeated calls from duplicating the report.
func v2MaterializeUnknown(owner string) {
	children, err := v2ListChildren(owner)
	if err != nil {
		return
	}
	for _, child := range children {
		if v2ChildUnknown(child, v2SupervisorLive(child.ChildID)) {
			v2MaybeReportUnknown(child, v2UnknownError)
		}
	}
}

// v2MaybeReportUnknown materializes the "lost/unknown always reports" policy
// once. The marker is removed when the child starts another turn or is reset
// by stop, so a later loss can report again.
func v2MaybeReportUnknown(child *v2Child, errorText string) {
	directory, err := v2ChildDir(child.ChildID)
	if err != nil {
		return
	}
	_ = v2WithLock(filepath.Join(directory, "unknown.lock"), func() error {
		marker := filepath.Join(directory, "unknown.report")
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
		if err := v2WriteText(marker, now()+"\n"); err != nil {
			return err
		}
		reportFile := ""
		if child.LastRunID != "" {
			reportFile = filepath.Join(directory, "runs", child.LastRunID, "result.json")
		}
		_, err := v2AppendInbox(child.OwnerID, map[string]any{
			"kind": "report", "agent": child.Name, "child_id": child.ChildID,
			"run_id": child.LastRunID, "session_id": child.SessionID, "outcome": "unknown",
			"report": child.Report, "text": "", "error": errorText,
			"created_at": now(), "report_file": reportFile,
		})
		return err
	})
}

func v2StartingExpired(child *v2Child) bool {
	created, err := time.Parse(time.RFC3339, child.CreatedAt)
	if err != nil {
		return true
	}
	return time.Since(created) > 10*time.Second
}

func v2SupervisorLive(childID string) bool {
	directory, err := v2ChildDir(childID)
	if err != nil {
		return false
	}
	file, err := os.OpenFile(filepath.Join(directory, "supervisor.lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	return flock(file, true) != nil
}

func v2Wait(caller v2Caller, args map[string]any) (map[string]any, error) {
	timeout := int(intArg(args, "timeout_seconds", v2DefaultWait))
	agent := stringField(args, "agent")
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		if caller.isChild() {
			if !time.Now().Before(deadline) {
				return map[string]any{"reports": []any{}, "timeout": true, "empty": false}, nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		v2MaterializeUnknown(caller.OwnerID)
		reports, unread := v2DrainInbox(caller.OwnerID, agent, v2InboxLimit)
		if len(reports) > 0 {
			return map[string]any{"reports": reports, "timeout": false, "empty": false}, nil
		}
		if unread == 0 && !v2HasOpenChildren(caller.OwnerID) {
			return map[string]any{"reports": []any{}, "timeout": false, "empty": true}, nil
		}
		if !time.Now().Before(deadline) {
			return map[string]any{"reports": []any{}, "timeout": true, "empty": false}, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func v2Stop(caller v2Caller, args map[string]any) (map[string]any, error) {
	if caller.isChild() {
		return nil, errors.New("stop is parent-only")
	}
	agent := stringField(args, "agent")
	id, err := v2ResolveName(caller.OwnerID, agent)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("unknown agent %q", agent)
	}
	child, err := v2ReadChild(id)
	if err != nil {
		return nil, err
	}
	if child.State == "closed" {
		return map[string]any{"agent": agent, "child_id": id, "dropped": 0, "state": "closed"}, nil
	}
	dropped, err := v2DropQueue(id)
	if err != nil {
		return nil, err
	}
	directory, err := v2ChildDir(id)
	if err != nil {
		return nil, err
	}
	if err := v2WriteText(filepath.Join(directory, "stop"), now()+"\n"); err != nil {
		return nil, err
	}
	lease, live, err := v2TryLease(id)
	if err != nil {
		return nil, err
	}
	if lease != nil {
		defer lease.Close()
		if _, err := v2UpdateChild(id, func(record *v2Child) error {
			record.State = "idle"
			record.LastOutcome = "cancelled"
			record.SupervisorPID = 0
			return nil
		}); err != nil {
			return nil, err
		}
		_ = os.Remove(filepath.Join(directory, "stop"))
		_ = os.Remove(filepath.Join(directory, "unknown.report"))
		return map[string]any{"agent": agent, "child_id": id, "dropped": dropped, "state": "idle"}, nil
	}
	if !live {
		return nil, fmt.Errorf("agent %q has no supervisor", agent)
	}
	if _, err := v2UpdateChild(id, func(record *v2Child) error {
		if record.State != "closed" {
			record.State = "stopping"
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return map[string]any{"agent": agent, "child_id": id, "dropped": dropped, "state": "stopping"}, nil
}

func v2Close(caller v2Caller, args map[string]any) (map[string]any, error) {
	if caller.isChild() {
		return nil, errors.New("close is parent-only")
	}
	agent := stringField(args, "agent")
	id, err := v2ResolveName(caller.OwnerID, agent)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("unknown agent %q", agent)
	}
	child, err := v2ReadChild(id)
	if err != nil {
		return nil, err
	}
	if child.State == "closed" {
		_ = v2RemoveName(caller.OwnerID, agent, id)
		return map[string]any{"agent": agent, "child_id": id, "dropped": 0, "closed": true, "state": "closed"}, nil
	}
	dropped, err := v2DropQueue(id)
	if err != nil {
		return nil, err
	}
	directory, err := v2ChildDir(id)
	if err != nil {
		return nil, err
	}
	if err := v2WriteText(filepath.Join(directory, "close"), now()+"\n"); err != nil {
		return nil, err
	}
	lease, live, err := v2TryLease(id)
	if err != nil {
		return nil, err
	}
	if lease != nil {
		defer lease.Close()
		if err := v2FinishClose(id, child.Name); err != nil {
			return nil, err
		}
		return map[string]any{"agent": agent, "child_id": id, "dropped": dropped, "closed": true, "state": "closed"}, nil
	}
	if !live {
		return nil, fmt.Errorf("agent %q has no supervisor", agent)
	}
	if _, err := v2UpdateChild(id, func(record *v2Child) error {
		if record.State != "closed" {
			record.State = "stopping"
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return map[string]any{"agent": agent, "child_id": id, "dropped": dropped, "closed": false, "state": "stopping"}, nil
}

func v2TryLease(childID string) (*os.File, bool, error) {
	directory, err := v2ChildDir(childID)
	if err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "supervisor.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	// A status probe holds this lease for only a few instructions. Retry
	// briefly so a message is not misread as having a live supervisor just
	// because it raced such a probe.
	for attempt := 0; attempt < 5; attempt++ {
		if err := flock(file, true); err == nil {
			return file, false, nil
		}
		if attempt < 4 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	file.Close()
	return nil, true, nil
}

func v2FinishClose(childID, agent string) error {
	_, err := v2UpdateChild(childID, func(record *v2Child) error {
		record.State = "closed"
		record.ClosedAt = now()
		record.SupervisorPID = 0
		return nil
	})
	if err != nil {
		return err
	}
	child, err := v2ReadChild(childID)
	if err != nil {
		return err
	}
	if _, err := v2DropQueue(childID); err != nil {
		return err
	}
	if err := v2RemoveName(child.OwnerID, agent, childID); err != nil {
		return err
	}
	if directory, err := v2ChildDir(childID); err == nil {
		_ = os.Remove(filepath.Join(directory, "stop"))
		_ = os.Remove(filepath.Join(directory, "close"))
	}
	return nil
}

func v2FinishStop(childID string) (int, error) {
	dropped, err := v2DropQueue(childID)
	if err != nil {
		return 0, err
	}
	if _, err := v2UpdateChild(childID, func(record *v2Child) error {
		record.State = "idle"
		record.LastOutcome = "cancelled"
		record.SupervisorPID = 0
		return nil
	}); err != nil {
		return 0, err
	}
	if directory, err := v2ChildDir(childID); err == nil {
		_ = os.Remove(filepath.Join(directory, "stop"))
		_ = os.Remove(filepath.Join(directory, "unknown.report"))
	}
	return dropped, nil
}

func v2SpawnSupervisor(childID string, sessionLock *os.File) (bool, error) {
	directory, err := v2ChildDir(childID)
	if err != nil {
		return false, err
	}
	lease, live, err := v2TryLease(childID)
	if err != nil {
		return false, err
	}
	if live {
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		lease.Close()
		return false, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		lease.Close()
		return false, err
	}
	logFile, err := os.OpenFile(filepath.Join(directory, "supervisor.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		lease.Close()
		return false, err
	}
	process := exec.Command(executable, "v2-supervisor", childID)
	process.Stderr = logFile
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.ExtraFiles = []*os.File{lease}
	environment := map[string]string{}
	if sessionLock != nil {
		process.ExtraFiles = append(process.ExtraFiles, sessionLock)
		environment["CAKE_SUBAGENT_SESSION_LOCK"] = "1"
	}
	if len(environment) > 0 {
		process.Env = withEnv(environment)
	}
	if err := process.Start(); err != nil {
		logFile.Close()
		lease.Close()
		return false, err
	}
	logFile.Close()
	lease.Close()
	return true, nil
}

func v2Execute(args map[string]any) (map[string]any, error) {
	action := stringField(args, "action")
	if action == "" {
		return nil, errors.New("action is required")
	}
	if err := v2ValidateRequest(action, args); err != nil {
		return nil, err
	}
	caller, err := v2CallerIdentity()
	if err != nil {
		return nil, err
	}
	if !caller.isChild() {
		v2MaterializeUnknown(caller.OwnerID)
	}
	var result map[string]any
	switch action {
	case "message":
		result, err = v2Message(caller, args)
	case "wait":
		result, err = v2Wait(caller, args)
	case "status":
		result, err = v2Status(caller, args)
	case "stop":
		result, err = v2Stop(caller, args)
	case "close":
		result, err = v2Close(caller, args)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return nil, err
	}
	// wait and status drain inside their own handlers so they can return the
	// reports they were waiting on. Every other successful parent action also
	// drains, which is the guaranteed delivery path while no hook is installed.
	if !caller.isChild() && action != "wait" && action != "status" {
		reports, _ := v2DrainInbox(caller.OwnerID, "", v2InboxLimit)
		result["reports"] = reports
	}
	return result, nil
}
