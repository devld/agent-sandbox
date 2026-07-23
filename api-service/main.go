package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultForegroundWait         = 120 * time.Second
	defaultInlineOutputLimit      = 64 * 1024
	defaultOutputTTL              = 12 * time.Hour
	defaultCleanupInterval        = 10 * time.Minute
	completedRecordRetention      = 10 * time.Minute
	defaultRequestBodyMaxSize     = 2 * 1024 * 1024
	defaultMCPInstructionsFile    = "/etc/agent-sandbox/mcp-instructions.md"
	defaultMCPInstructions        = "Use exec for shell, JavaScript, and Python. Poll long-running tasks with exec_status; use exec_list and exec_kill to manage them."
	defaultMCPBrowserInstructions = "Browser automation is available through playwright-cli via exec; use it to open pages, inspect snapshots, interact with elements, and take screenshots."
)

type config struct {
	host              string
	port              string
	token             string
	username          string
	uid               int
	gid               int
	home              string
	inlineOutputLimit int64
	outputDir         string
	outputTTL         time.Duration
	cleanupInterval   time.Duration
	browserDisabled   bool
	mcpInstructions   string
}

type executionRequest struct {
	Script           string            `json:"script"`
	Mode             string            `json:"mode,omitempty"`
	Cwd              string            `json:"cwd,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	ForegroundWaitMS *int64            `json:"foregroundWaitMs,omitempty"`
}

type taskIDRequest struct {
	TaskID string `json:"taskId"`
}

type taskListRequest struct {
	Status string `json:"status,omitempty"`
}

type outputView struct {
	Type      string     `json:"type"`
	Content   string     `json:"content,omitempty"`
	Path      string     `json:"path,omitempty"`
	Bytes     int64      `json:"bytes"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	ExpiredAt *time.Time `json:"expiredAt,omitempty"`
}

type taskView struct {
	TaskID          string     `json:"taskId,omitempty"`
	Status          string     `json:"status"`
	Mode            string     `json:"mode,omitempty"`
	Cwd             string     `json:"cwd,omitempty"`
	StartedAt       time.Time  `json:"startedAt,omitempty"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
	ObservedAt      *time.Time `json:"observedAt,omitempty"`
	RecordExpiresAt *time.Time `json:"recordExpiresAt,omitempty"`
	DurationMS      int64      `json:"durationMs,omitempty"`
	ExitCode        *int       `json:"exitCode,omitempty"`
	Signal          string     `json:"signal,omitempty"`
	Killed          bool       `json:"killed,omitempty"`
	Error           string     `json:"error,omitempty"`
	Stdout          outputView `json:"stdout"`
	Stderr          outputView `json:"stderr"`
}

type taskSummary struct {
	TaskID          string     `json:"taskId"`
	Status          string     `json:"status"`
	Mode            string     `json:"mode"`
	Cwd             string     `json:"cwd"`
	ScriptPreview   string     `json:"scriptPreview"`
	StartedAt       time.Time  `json:"startedAt"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
	RecordExpiresAt *time.Time `json:"recordExpiresAt,omitempty"`
}

type taskListView struct {
	Tasks []taskSummary `json:"tasks"`
}

type asyncExecutionView struct {
	TaskID string `json:"taskId"`
	Status string `json:"status"`
}

type killView struct {
	TaskID           string    `json:"taskId"`
	Status           string    `json:"status"`
	Killed           bool      `json:"killed"`
	AlreadyCompleted bool      `json:"alreadyCompleted,omitempty"`
	Result           *taskView `json:"result,omitempty"`
}

type taskOutput struct {
	mu          sync.Mutex
	name        string
	taskDir     string
	inlineLimit int64
	uid         int
	gid         int
	buffer      bytes.Buffer
	file        *os.File
	path        string
	bytes       int64
	writeErr    error
}

func (output *taskOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()

	originalLength := len(data)
	output.bytes += int64(originalLength)
	if output.writeErr != nil {
		return originalLength, nil
	}
	if output.file == nil && int64(output.buffer.Len()+originalLength) <= output.inlineLimit {
		_, output.writeErr = output.buffer.Write(data)
		return originalLength, output.writeErr
	}
	if output.file == nil {
		if err := output.openFile(); err != nil {
			output.writeErr = err
			return originalLength, nil
		}
		if output.buffer.Len() > 0 {
			if _, err := output.file.Write(output.buffer.Bytes()); err != nil {
				output.writeErr = err
				return originalLength, nil
			}
			output.buffer.Reset()
		}
	}
	if _, err := output.file.Write(data); err != nil {
		output.writeErr = err
	}
	return originalLength, nil
}

func (output *taskOutput) openFile() error {
	if err := os.MkdirAll(output.taskDir, 0o700); err != nil {
		return err
	}
	if err := os.Chown(output.taskDir, output.uid, output.gid); err != nil {
		return err
	}
	output.path = filepath.Join(output.taskDir, output.name)
	file, err := os.OpenFile(output.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chown(output.uid, output.gid); err != nil {
		_ = file.Close()
		return err
	}
	output.file = file
	return nil
}

func (output *taskOutput) close() {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.file != nil {
		_ = output.file.Close()
		output.file = nil
	}
}

func (output *taskOutput) failure() error {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.writeErr
}

func (output *taskOutput) snapshot(completedAt *time.Time, outputTTL time.Duration) outputView {
	output.mu.Lock()
	defer output.mu.Unlock()

	if output.path == "" {
		return outputView{Type: "inline", Content: output.buffer.String(), Bytes: output.bytes}
	}
	view := outputView{Type: "file", Path: output.path, Bytes: output.bytes}
	if completedAt != nil {
		expiresAt := completedAt.Add(outputTTL)
		view.ExpiresAt = &expiresAt
		if _, err := os.Stat(output.path); errors.Is(err, os.ErrNotExist) {
			view.Type = "expired"
			view.Path = ""
			view.ExpiredAt = &expiresAt
		}
	}
	return view
}

type executionTask struct {
	mu              sync.RWMutex
	id              string
	mode            string
	script          string
	cwd             string
	startedAt       time.Time
	completedAt     *time.Time
	observedAt      *time.Time
	recordExpiresAt *time.Time
	status          string
	exitCode        *int
	signal          string
	killed          bool
	runError        string
	command         *exec.Cmd
	stdout          *taskOutput
	stderr          *taskOutput
	done            chan struct{}
	killOnce        sync.Once
	outputTTL       time.Duration
	taskDir         string
}

func (task *executionTask) view(includeID bool) taskView {
	task.mu.RLock()
	defer task.mu.RUnlock()
	view := taskView{
		Status:          task.status,
		Mode:            task.mode,
		Cwd:             task.cwd,
		StartedAt:       task.startedAt,
		CompletedAt:     task.completedAt,
		ObservedAt:      task.observedAt,
		RecordExpiresAt: task.recordExpiresAt,
		ExitCode:        task.exitCode,
		Signal:          task.signal,
		Killed:          task.killed,
		Error:           task.runError,
		Stdout:          task.stdout.snapshot(task.completedAt, task.outputTTL),
		Stderr:          task.stderr.snapshot(task.completedAt, task.outputTTL),
	}
	if includeID {
		view.TaskID = task.id
	}
	if task.completedAt != nil {
		view.DurationMS = task.completedAt.Sub(task.startedAt).Milliseconds()
	}
	return view
}

func (task *executionTask) summary() taskSummary {
	task.mu.RLock()
	defer task.mu.RUnlock()
	return taskSummary{
		TaskID:          task.id,
		Status:          task.status,
		Mode:            task.mode,
		Cwd:             task.cwd,
		ScriptPreview:   preview(task.script, 200),
		StartedAt:       task.startedAt,
		CompletedAt:     task.completedAt,
		RecordExpiresAt: task.recordExpiresAt,
	}
}

func (task *executionTask) observe(now time.Time) (taskView, bool) {
	newlyObserved := false
	task.mu.Lock()
	if task.status == "completed" && task.observedAt == nil {
		observedAt := now
		recordExpiresAt := now.Add(completedRecordRetention)
		task.observedAt = &observedAt
		task.recordExpiresAt = &recordExpiresAt
		newlyObserved = true
	}
	task.mu.Unlock()
	return task.view(true), newlyObserved
}

func (task *executionTask) kill() killView {
	task.mu.RLock()
	alreadyCompleted := task.status == "completed"
	task.mu.RUnlock()
	if alreadyCompleted {
		result := task.view(true)
		return killView{
			TaskID:           task.id,
			Status:           "completed",
			AlreadyCompleted: true,
			Result:           &result,
		}
	}

	task.killOnce.Do(func() {
		task.mu.Lock()
		task.killed = true
		process := task.command.Process
		task.mu.Unlock()
		if process == nil {
			return
		}
		_ = syscall.Kill(-process.Pid, syscall.SIGTERM)
		select {
		case <-task.done:
		case <-time.After(time.Second):
			_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
			<-task.done
		}
	})
	result := task.view(true)
	return killView{
		TaskID: task.id,
		Status: result.Status,
		Killed: true,
		Result: &result,
	}
}

type taskManager struct {
	cfg   config
	mu    sync.RWMutex
	tasks map[string]*executionTask
	next  atomic.Uint64
	now   func() time.Time
}

func newTaskManager(cfg config) *taskManager {
	manager := &taskManager{
		cfg:   cfg,
		tasks: make(map[string]*executionTask),
		now:   time.Now,
	}
	manager.next.Store(uint64(time.Now().UnixMilli()))
	return manager
}

func (manager *taskManager) nextID() string {
	return "t" + strconv.FormatUint(manager.next.Add(1), 10)
}

func (manager *taskManager) start(request executionRequest) (*executionTask, error) {
	mode := request.Mode
	if mode == "" {
		mode = "shell"
	}
	if mode != "shell" && mode != "javascript" && mode != "python" {
		return nil, errors.New("mode must be shell, javascript, or python")
	}
	if request.Script == "" {
		return nil, errors.New("script must be a non-empty string")
	}
	cwd := request.Cwd
	if cwd == "" {
		cwd = manager.cfg.home
	}
	if !filepath.IsAbs(cwd) {
		return nil, errors.New("cwd must be an absolute path")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, errors.New("cwd must be an existing directory")
	}
	environment, err := safeEnvironment(manager.cfg, request.Env)
	if err != nil {
		return nil, err
	}

	shellCommand := request.Script
	stdin := ""
	switch mode {
	case "javascript":
		shellCommand = "node --input-type=module -"
		stdin = request.Script
	case "python":
		shellCommand = "python -"
		stdin = request.Script
	}
	var command *exec.Cmd
	if os.Geteuid() != 0 && os.Geteuid() == manager.cfg.uid && os.Getegid() == manager.cfg.gid {
		// Local tests and development may run the service as the target user.
		// Production starts it as root and always takes the setpriv branch.
		command = exec.Command("/bin/bash", "--noprofile", "--norc", "-c", shellCommand)
	} else {
		args := []string{
			fmt.Sprintf("--reuid=%d", manager.cfg.uid),
			fmt.Sprintf("--regid=%d", manager.cfg.gid),
			"--init-groups",
			"--",
			"/bin/bash",
			"-lc",
			shellCommand,
		}
		command = exec.Command("setpriv", args...)
	}
	command.Dir = cwd
	command.Env = environment
	command.Stdin = strings.NewReader(stdin)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	id := manager.nextID()
	taskDir := filepath.Join(manager.cfg.outputDir, id)
	task := &executionTask{
		id:        id,
		mode:      mode,
		script:    request.Script,
		cwd:       cwd,
		startedAt: manager.now().UTC(),
		status:    "running",
		command:   command,
		done:      make(chan struct{}),
		outputTTL: manager.cfg.outputTTL,
		taskDir:   taskDir,
	}
	task.stdout = &taskOutput{
		name:        "stdout",
		taskDir:     taskDir,
		inlineLimit: manager.cfg.inlineOutputLimit,
		uid:         manager.cfg.uid,
		gid:         manager.cfg.gid,
	}
	task.stderr = &taskOutput{
		name:        "stderr",
		taskDir:     taskDir,
		inlineLimit: manager.cfg.inlineOutputLimit,
		uid:         manager.cfg.uid,
		gid:         manager.cfg.gid,
	}
	command.Stdout = task.stdout
	command.Stderr = task.stderr

	manager.mu.Lock()
	manager.tasks[id] = task
	manager.mu.Unlock()
	if err := command.Start(); err != nil {
		manager.remove(id)
		return nil, err
	}
	go manager.wait(task)
	return task, nil
}

func (manager *taskManager) wait(task *executionTask) {
	waitErr := task.command.Wait()
	task.stdout.close()
	task.stderr.close()
	completedAt := manager.now().UTC()

	task.mu.Lock()
	task.status = "completed"
	task.completedAt = &completedAt
	if task.command.ProcessState != nil {
		exitCode := task.command.ProcessState.ExitCode()
		task.exitCode = &exitCode
		if status, ok := task.command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			task.signal = status.Signal().String()
		}
	}
	var exitError *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitError) {
		task.runError = waitErr.Error()
	}
	for _, output := range []*taskOutput{task.stdout, task.stderr} {
		if outputErr := output.failure(); outputErr != nil {
			if task.runError != "" {
				task.runError += "; "
			}
			task.runError += "capturing output: " + outputErr.Error()
		}
	}
	task.mu.Unlock()

	manager.markOutputCompleted(task)
	close(task.done)
}

func (manager *taskManager) markOutputCompleted(task *executionTask) {
	if _, err := os.Stat(task.taskDir); err != nil {
		return
	}
	marker := filepath.Join(task.taskDir, ".completed")
	if err := os.WriteFile(marker, []byte(task.completedAt.Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return
	}
	_ = os.Chtimes(marker, *task.completedAt, *task.completedAt)
}

func (manager *taskManager) execute(request executionRequest) (any, int, error) {
	wait := defaultForegroundWait
	if request.ForegroundWaitMS != nil {
		if *request.ForegroundWaitMS < 0 {
			return nil, 0, errors.New("foregroundWaitMs must be zero or a positive integer")
		}
		if *request.ForegroundWaitMS > math.MaxInt64/int64(time.Millisecond) {
			return nil, 0, errors.New("foregroundWaitMs is too large")
		}
		wait = time.Duration(*request.ForegroundWaitMS) * time.Millisecond
	}
	task, err := manager.start(request)
	if err != nil {
		return nil, 0, err
	}
	if wait == 0 {
		return asyncExecutionView{TaskID: task.id, Status: "running"}, http.StatusAccepted, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-task.done:
		view := task.view(false)
		manager.remove(task.id)
		return view, http.StatusOK, nil
	case <-timer.C:
		return asyncExecutionView{TaskID: task.id, Status: task.view(true).Status}, http.StatusAccepted, nil
	}
}

func (manager *taskManager) status(id string, observe bool) (taskView, bool) {
	manager.mu.RLock()
	task, ok := manager.tasks[id]
	manager.mu.RUnlock()
	if !ok {
		return taskView{}, false
	}
	if observe {
		view, newlyObserved := task.observe(manager.now().UTC())
		if newlyObserved && view.RecordExpiresAt != nil {
			delay := time.Until(*view.RecordExpiresAt)
			if delay < 0 {
				delay = 0
			}
			time.AfterFunc(delay, func() {
				manager.removeExpired(id, time.Now().UTC())
			})
		}
		return view, true
	}
	return task.view(true), true
}

func (manager *taskManager) list(status string) (taskListView, error) {
	if status != "" && status != "running" && status != "completed" {
		return taskListView{}, errors.New("status must be running or completed")
	}
	manager.mu.RLock()
	tasks := make([]*executionTask, 0, len(manager.tasks))
	for _, task := range manager.tasks {
		tasks = append(tasks, task)
	}
	manager.mu.RUnlock()

	summaries := make([]taskSummary, 0, len(tasks))
	for _, task := range tasks {
		summary := task.summary()
		if status == "" || summary.Status == status {
			summaries = append(summaries, summary)
		}
	}
	sort.Slice(summaries, func(left, right int) bool {
		return summaries[left].StartedAt.Before(summaries[right].StartedAt)
	})
	return taskListView{Tasks: summaries}, nil
}

func (manager *taskManager) kill(id string) (killView, bool) {
	manager.mu.RLock()
	task, ok := manager.tasks[id]
	manager.mu.RUnlock()
	if !ok {
		return killView{}, false
	}
	return task.kill(), true
}

func (manager *taskManager) remove(id string) {
	manager.mu.Lock()
	delete(manager.tasks, id)
	manager.mu.Unlock()
}

func (manager *taskManager) removeExpired(id string, now time.Time) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	task, ok := manager.tasks[id]
	if !ok {
		return
	}
	task.mu.RLock()
	expiresAt := task.recordExpiresAt
	task.mu.RUnlock()
	if expiresAt != nil && !now.Before(*expiresAt) {
		delete(manager.tasks, id)
	}
}

func (manager *taskManager) cleanup(now time.Time) {
	manager.mu.Lock()
	for id, task := range manager.tasks {
		task.mu.RLock()
		expiresAt := task.recordExpiresAt
		task.mu.RUnlock()
		if expiresAt != nil && !now.Before(*expiresAt) {
			delete(manager.tasks, id)
		}
	}
	manager.mu.Unlock()

	entries, err := os.ReadDir(manager.cfg.outputDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskDir := filepath.Join(manager.cfg.outputDir, entry.Name())
		markerInfo, err := os.Stat(filepath.Join(taskDir, ".completed"))
		if err != nil {
			manager.mu.RLock()
			_, runningOrRetained := manager.tasks[entry.Name()]
			manager.mu.RUnlock()
			if runningOrRetained {
				continue
			}
			directoryInfo, directoryErr := entry.Info()
			if directoryErr == nil && !now.Before(directoryInfo.ModTime().Add(manager.cfg.outputTTL)) {
				_ = os.RemoveAll(taskDir)
			}
			continue
		}
		if !now.Before(markerInfo.ModTime().Add(manager.cfg.outputTTL)) {
			_ = os.RemoveAll(taskDir)
		}
	}
}

func (manager *taskManager) runCleanup(ctx context.Context) {
	manager.cleanup(manager.now().UTC())
	ticker := time.NewTicker(manager.cfg.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			manager.cleanup(now.UTC())
		}
	}
}

func (manager *taskManager) killAll() {
	manager.mu.RLock()
	tasks := make([]*executionTask, 0, len(manager.tasks))
	for _, task := range manager.tasks {
		tasks = append(tasks, task)
	}
	manager.mu.RUnlock()
	for _, task := range tasks {
		task.kill()
	}
}

func preview(value string, maximum int) string {
	value = strings.ReplaceAll(value, "\n", " ")
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum]) + "…"
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

func loadConfig() (config, error) {
	username := getenv("SANDBOX_USERNAME", "agent")
	browserDisabled, err := envBool("SANDBOX_BROWSER_DISABLED", false)
	if err != nil {
		return config{}, err
	}
	mcpInstructions, err := loadMCPInstructions(!browserDisabled)
	if err != nil {
		return config{}, err
	}
	return config{
		host:              getenv("SANDBOX_API_HOST", "0.0.0.0"),
		port:              getenv("SANDBOX_API_PORT", "3000"),
		token:             os.Getenv("SANDBOX_API_TOKEN"),
		username:          username,
		uid:               envInt("SANDBOX_UID", 1000),
		gid:               envInt("SANDBOX_GID", 1000),
		home:              getenv("SANDBOX_HOME", "/home/"+username),
		inlineOutputLimit: envInt64("EXEC_INLINE_OUTPUT_LIMIT_BYTES", defaultInlineOutputLimit),
		outputDir:         getenv("EXEC_OUTPUT_DIR", "/tmp/agent-sandbox/tasks"),
		outputTTL:         envDuration("EXEC_OUTPUT_TTL", defaultOutputTTL),
		cleanupInterval:   envDuration("EXEC_CLEANUP_INTERVAL", defaultCleanupInterval),
		browserDisabled:   browserDisabled,
		mcpInstructions:   mcpInstructions,
	}, nil
}

func envBool(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	switch strings.ToLower(value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
}

func defaultMCPInstructionsFor(browserEnabled bool) string {
	if browserEnabled {
		return defaultMCPInstructions + " " + defaultMCPBrowserInstructions
	}
	return defaultMCPInstructions
}

func loadMCPInstructions(browserEnabled bool) (string, error) {
	return loadMCPInstructionsFile(defaultMCPInstructionsFile, browserEnabled)
}

func loadMCPInstructionsFile(path string, browserEnabled bool) (string, error) {
	defaults := defaultMCPInstructionsFor(browserEnabled)
	instructions, err := os.ReadFile(path)
	if err == nil {
		if len(bytes.TrimSpace(instructions)) == 0 {
			return defaults, nil
		}
		return defaults + "\n\n" + string(instructions), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return defaults, nil
	}
	return "", fmt.Errorf("read MCP server instructions from %s: %w", path, err)
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func safeEnvironment(cfg config, extra map[string]string) ([]string, error) {
	allowed := []string{"DISPLAY", "LANG", "LC_ALL", "PATH", "TERM", "TZ"}
	environment := make([]string, 0, len(allowed)+len(extra)+5)
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	environment = append(environment,
		"HOME="+cfg.home,
		"USER="+cfg.username,
		"LOGNAME="+cfg.username,
		"SHELL=/bin/bash",
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", cfg.uid),
	)
	for key, value := range extra {
		if key == "SANDBOX_API_TOKEN" {
			return nil, errors.New("SANDBOX_API_TOKEN cannot be passed to child processes")
		}
		if !validEnvironmentName(key) || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("env must contain valid environment names and string values")
		}
		environment = append(environment, key+"="+value)
	}
	return environment, nil
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func decodeJSON(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, defaultRequestBodyMaxSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func authenticated(cfg config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if cfg.token == "" {
			next.ServeHTTP(response, request)
			return
		}
		expected := "Bearer " + cfg.token
		supplied := request.Header.Get("Authorization")
		if len(supplied) != len(expected) ||
			subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) != 1 {
			writeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(response, request)
	})
}

func execHandler(manager *taskManager) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			response.Header().Set("Allow", http.MethodPost)
			writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		var input executionRequest
		if err := decodeJSON(request, &input); err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, status, err := manager.execute(input)
		if err != nil {
			writeJSON(response, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(response, status, result)
	}
}

func execTasksHandler(manager *taskManager) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		result, err := manager.list(request.URL.Query().Get("status"))
		if err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(response, http.StatusOK, result)
	}
}

func execTaskHandler(manager *taskManager) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		id := strings.TrimPrefix(request.URL.Path, "/exec/tasks/")
		if id == "" || strings.Contains(id, "/") {
			writeJSON(response, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		switch request.Method {
		case http.MethodGet:
			result, ok := manager.status(id, true)
			if !ok {
				writeJSON(response, http.StatusNotFound, map[string]string{"error": "task not found"})
				return
			}
			writeJSON(response, http.StatusOK, result)
		case http.MethodDelete:
			result, ok := manager.kill(id)
			if !ok {
				writeJSON(response, http.StatusNotFound, map[string]string{"error": "task not found"})
				return
			}
			writeJSON(response, http.StatusOK, result)
		default:
			response.Header().Set("Allow", "GET, DELETE")
			writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "exec",
			"description": "Run shell, JavaScript, or Python as the unprivileged sandbox user. Tasks that outlive foregroundWaitMs continue in the background.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"script":           map[string]any{"type": "string", "minLength": 1},
					"mode":             map[string]any{"type": "string", "enum": []string{"shell", "javascript", "python"}, "default": "shell"},
					"foregroundWaitMs": map[string]any{"type": "integer", "minimum": 0, "default": defaultForegroundWait.Milliseconds()},
					"cwd":              map[string]any{"type": "string", "description": "Absolute working directory; defaults to the sandbox home"},
					"env":              map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				},
				"required":             []string{"script"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "exec_status",
			"description": "Get live stdout/stderr and status for a background exec task. The first completed status read starts its 10-minute record retention.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"taskId": map[string]any{"type": "string", "pattern": "^t[0-9]+$"}},
				"required":             []string{"taskId"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "exec_list",
			"description": "List running and retained completed exec tasks without consuming completed results.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"running", "completed"}},
				},
				"additionalProperties": false,
			},
		},
		{
			"name":        "exec_kill",
			"description": "Terminate an exec task and its process group. Sends SIGTERM, then SIGKILL after one second if needed.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"taskId": map[string]any{"type": "string", "pattern": "^t[0-9]+$"}},
				"required":             []string{"taskId"},
				"additionalProperties": false,
			},
		},
	}
}

func mcpHandler(manager *taskManager) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			response.Header().Set("Allow", http.MethodPost)
			writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "stateless server accepts POST only"})
			return
		}
		var rpc rpcRequest
		if err := decodeJSON(request, &rpc); err != nil {
			writeJSON(response, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}})
			return
		}
		if len(rpc.ID) == 0 {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		result, rpcErr := handleRPC(manager, rpc)
		writeJSON(response, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: rpc.ID, Result: result, Error: rpcErr})
	}
}

func decodeRPCArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func mcpToolResult(value any, isError bool) map[string]any {
	encoded, _ := json.Marshal(value)
	return map[string]any{
		"content":           []map[string]string{{"type": "text", "text": string(encoded)}},
		"structuredContent": value,
		"isError":           isError,
	}
}

func mcpToolError(err error) map[string]any {
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}

func handleRPC(manager *taskManager, rpc rpcRequest) (any, *rpcError) {
	switch rpc.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(rpc.Params, &params)
		version := params.ProtocolVersion
		if version != "2025-11-25" && version != "2025-06-18" && version != "2025-03-26" {
			version = "2025-11-25"
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]bool{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "agent-sandbox", "version": "0.1.0"},
			"instructions":    manager.cfg.mcpInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefinitions()}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(rpc.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		switch params.Name {
		case "exec":
			var input executionRequest
			if err := decodeRPCArguments(params.Arguments, &input); err != nil {
				return mcpToolError(err), nil
			}
			result, _, err := manager.execute(input)
			if err != nil {
				return mcpToolError(err), nil
			}
			isError := false
			if completed, ok := result.(taskView); ok && completed.ExitCode != nil {
				isError = *completed.ExitCode != 0
			}
			return mcpToolResult(result, isError), nil
		case "exec_status":
			var input taskIDRequest
			if err := decodeRPCArguments(params.Arguments, &input); err != nil {
				return mcpToolError(err), nil
			}
			result, ok := manager.status(input.TaskID, true)
			if !ok {
				return mcpToolError(errors.New("task not found")), nil
			}
			return mcpToolResult(result, false), nil
		case "exec_list":
			var input taskListRequest
			if err := decodeRPCArguments(params.Arguments, &input); err != nil {
				return mcpToolError(err), nil
			}
			result, err := manager.list(input.Status)
			if err != nil {
				return mcpToolError(err), nil
			}
			return mcpToolResult(result, false), nil
		case "exec_kill":
			var input taskIDRequest
			if err := decodeRPCArguments(params.Arguments, &input); err != nil {
				return mcpToolError(err), nil
			}
			result, ok := manager.kill(input.TaskID)
			if !ok {
				return mcpToolError(errors.New("task not found")), nil
			}
			return mcpToolResult(result, false), nil
		default:
			return nil, &rpcError{Code: -32602, Message: "unknown tool"}
		}
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func newHTTPHandler(cfg config, manager *taskManager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	protected := http.NewServeMux()
	protected.HandleFunc("/exec", execHandler(manager))
	protected.HandleFunc("/exec/tasks", execTasksHandler(manager))
	protected.HandleFunc("/exec/tasks/", execTaskHandler(manager))
	protected.HandleFunc("/mcp", mcpHandler(manager))
	mux.Handle("/", authenticated(cfg, protected))
	return mux
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	manager := newTaskManager(cfg)
	cleanupContext, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	go manager.runCleanup(cleanupContext)

	server := &http.Server{
		Addr:              cfg.host + ":" + cfg.port,
		Handler:           newHTTPHandler(cfg, manager),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("listening on http://%s; commands run as %s (%d:%d), service uid=%d", server.Addr, cfg.username, cfg.uid, cfg.gid, os.Getuid())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopCleanup()
	manager.killAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
