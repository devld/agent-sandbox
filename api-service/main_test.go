package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) config {
	t.Helper()
	username := "test"
	if current, err := user.Current(); err == nil {
		username = current.Username
	}
	return config{
		username:          username,
		uid:               os.Getuid(),
		gid:               os.Getgid(),
		home:              t.TempDir(),
		inlineOutputLimit: 64,
		outputDir:         t.TempDir(),
		outputTTL:         12 * time.Hour,
		cleanupInterval:   time.Hour,
	}
}

func milliseconds(value int64) *int64 {
	return &value
}

func TestSafeEnvironmentDoesNotInheritToken(t *testing.T) {
	t.Setenv("SANDBOX_API_TOKEN", "secret")
	environment, err := safeEnvironment(config{
		username: "agent",
		uid:      1000,
		home:     "/home/agent",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range environment {
		if strings.HasPrefix(entry, "SANDBOX_API_TOKEN=") {
			t.Fatal("root API token leaked into child environment")
		}
	}
}

func TestTaskOutputSpillsCompleteStreamToFile(t *testing.T) {
	taskDir := filepath.Join(t.TempDir(), "t1")
	output := &taskOutput{
		name:        "stdout",
		taskDir:     taskDir,
		inlineLimit: 4,
		uid:         os.Getuid(),
		gid:         os.Getgid(),
	}
	if _, err := output.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	inline := output.snapshot(nil, time.Hour)
	if inline.Type != "inline" || inline.Content != "abc" {
		t.Fatalf("unexpected inline output: %#v", inline)
	}
	if _, err := output.Write([]byte("de")); err != nil {
		t.Fatal(err)
	}
	output.close()
	spilled := output.snapshot(nil, time.Hour)
	if spilled.Type != "file" {
		t.Fatalf("expected file output, got %#v", spilled)
	}
	content, err := os.ReadFile(spilled.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "abcde" {
		t.Fatalf("spill file lost buffered prefix: %q", content)
	}
}

func TestSynchronousShellExecutionIsRemovedFromTaskList(t *testing.T) {
	manager := newTaskManager(testConfig(t))
	result, status, err := manager.execute(executionRequest{
		Script:           `printf 'hello'`,
		ForegroundWaitMS: milliseconds(5_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	view, ok := result.(taskView)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	if view.TaskID != "" || view.Status != "completed" || view.Stdout.Content != "hello" {
		t.Fatalf("unexpected result: %#v", view)
	}
	tasks, err := manager.list("")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks.Tasks) != 0 {
		t.Fatalf("synchronous task was retained: %#v", tasks.Tasks)
	}
}

func TestJavaScriptAndPythonModesSendScriptOnStdin(t *testing.T) {
	cfg := testConfig(t)
	binDir := t.TempDir()
	for _, executable := range []string{"node", "python"} {
		path := filepath.Join(binDir, executable)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '"+executable+":'\ncat\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	manager := newTaskManager(cfg)
	for _, test := range []struct {
		mode   string
		script string
		want   string
	}{
		{mode: "javascript", script: "console.log(42)", want: "node:console.log(42)"},
		{mode: "python", script: "print(42)", want: "python:print(42)"},
	} {
		result, status, err := manager.execute(executionRequest{
			Mode:             test.mode,
			Script:           test.script,
			ForegroundWaitMS: milliseconds(5_000),
		})
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK {
			t.Fatalf("%s returned status %d", test.mode, status)
		}
		view := result.(taskView)
		if view.ExitCode == nil || *view.ExitCode != 0 || view.Stdout.Content != test.want {
			t.Fatalf("unexpected %s result: %#v", test.mode, view)
		}
	}
}

func TestAsyncStatusListKillAndObservedCleanup(t *testing.T) {
	manager := newTaskManager(testConfig(t))
	result, status, err := manager.execute(executionRequest{
		Script:           `printf 'started\n'; sleep 30`,
		ForegroundWaitMS: milliseconds(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", status)
	}
	async, ok := result.(asyncExecutionView)
	if !ok || !strings.HasPrefix(async.TaskID, "t") {
		t.Fatalf("unexpected async result: %#v", result)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		view, found := manager.status(async.TaskID, false)
		if !found {
			t.Fatal("task disappeared")
		}
		if strings.Contains(view.Stdout.Content, "started") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live stdout was not available: %#v", view.Stdout)
		}
		time.Sleep(10 * time.Millisecond)
	}

	list, err := manager.list("running")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].TaskID != async.TaskID {
		t.Fatalf("running task missing from list: %#v", list)
	}

	killed, found := manager.kill(async.TaskID)
	if !found || !killed.Killed || killed.Status != "completed" {
		t.Fatalf("unexpected kill result: %#v", killed)
	}
	completed, found := manager.status(async.TaskID, true)
	if !found || completed.ObservedAt == nil || completed.RecordExpiresAt == nil {
		t.Fatalf("completed observation did not start retention: %#v", completed)
	}
	manager.cleanup(completed.RecordExpiresAt.Add(time.Nanosecond))
	if _, found := manager.status(async.TaskID, false); found {
		t.Fatal("observed completed task was not removed after retention")
	}
}

func TestOutputCleanupUsesCompletionMarker(t *testing.T) {
	cfg := testConfig(t)
	cfg.outputTTL = time.Hour
	manager := newTaskManager(cfg)
	taskDir := filepath.Join(cfg.outputDir, "t123")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "stdout"), []byte("large output"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(taskDir, ".completed")
	if err := os.WriteFile(marker, []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(marker, completedAt, completedAt); err != nil {
		t.Fatal(err)
	}
	manager.cleanup(time.Now())
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("expired output directory still exists: %v", err)
	}
}

func TestLoadMCPInstructionsPrependsDefaultsToCustomFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-instructions.md")
	custom := "Use the browser only when requested.\n"
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	instructions, err := loadMCPInstructionsFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultMCPInstructionsFor(true) + "\n\n" + custom
	if instructions != want {
		t.Fatalf("unexpected instructions %q", instructions)
	}
}

func TestLoadMCPInstructionsUsesDefaultsWithoutCustomFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.md")

	instructions, err := loadMCPInstructionsFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if instructions != defaultMCPInstructionsFor(true) {
		t.Fatalf("unexpected instructions %q", instructions)
	}
	if !strings.Contains(instructions, "playwright-cli") {
		t.Fatal("default instructions do not advertise playwright-cli")
	}
}

func TestLoadMCPInstructionsUsesDefaultsForEmptyCustomFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-instructions.md")
	if err := os.WriteFile(path, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}

	instructions, err := loadMCPInstructionsFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if instructions != defaultMCPInstructionsFor(true) {
		t.Fatalf("unexpected instructions %q", instructions)
	}
}

func TestLoadMCPInstructionsOmitsBrowserDefaultsWhenDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-instructions.md")
	custom := "Keep this custom instruction.\n"
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	instructions, err := loadMCPInstructionsFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultMCPInstructions + "\n\n" + custom
	if instructions != want {
		t.Fatalf("unexpected instructions %q", instructions)
	}
	if strings.Contains(instructions, "playwright-cli") {
		t.Fatal("disabled browser instructions still advertise playwright-cli")
	}
}

func TestBrowserDisabledDefaultsToFalseAndValidatesInput(t *testing.T) {
	t.Setenv("SANDBOX_BROWSER_DISABLED", "")
	disabled, err := envBool("SANDBOX_BROWSER_DISABLED", false)
	if err != nil || disabled {
		t.Fatalf("expected enabled-by-default browser, got disabled=%t err=%v", disabled, err)
	}

	t.Setenv("SANDBOX_BROWSER_DISABLED", "true")
	disabled, err = envBool("SANDBOX_BROWSER_DISABLED", false)
	if err != nil || !disabled {
		t.Fatalf("expected disabled value, got disabled=%t err=%v", disabled, err)
	}

	t.Setenv("SANDBOX_BROWSER_DISABLED", "sometimes")
	if _, err := envBool("SANDBOX_BROWSER_DISABLED", false); err == nil {
		t.Fatal("invalid browser disabled value was accepted")
	}
}

func TestLoadConfigBrowserFlagControlsDefaultInstructions(t *testing.T) {
	for _, test := range []struct {
		value           string
		wantDisabled    bool
		wantInstruction bool
	}{
		{value: "false", wantDisabled: false, wantInstruction: true},
		{value: "true", wantDisabled: true, wantInstruction: false},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("SANDBOX_BROWSER_DISABLED", test.value)
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.browserDisabled != test.wantDisabled {
				t.Fatalf("browserDisabled=%t, want %t", cfg.browserDisabled, test.wantDisabled)
			}
			hasBrowserInstructions := strings.Contains(cfg.mcpInstructions, "playwright-cli")
			if hasBrowserInstructions != test.wantInstruction {
				t.Fatalf("browser instructions present=%t, want %t: %q", hasBrowserInstructions, test.wantInstruction, cfg.mcpInstructions)
			}
		})
	}
}

func TestMCPInitializeAndTools(t *testing.T) {
	cfg := testConfig(t)
	cfg.mcpInstructions = "Configured MCP instructions."
	manager := newTaskManager(cfg)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	response := httptest.NewRecorder()
	mcpHandler(manager).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Instructions    string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Result.ProtocolVersion != "2025-11-25" {
		t.Fatalf("unexpected protocol version %q", payload.Result.ProtocolVersion)
	}
	if payload.Result.Instructions != cfg.mcpInstructions {
		t.Fatalf("unexpected instructions %q", payload.Result.Instructions)
	}

	names := make(map[string]bool)
	for _, definition := range toolDefinitions() {
		names[definition["name"].(string)] = true
	}
	for _, expected := range []string{"exec", "exec_status", "exec_list", "exec_kill"} {
		if !names[expected] {
			t.Errorf("missing MCP tool %s", expected)
		}
	}
	if names["run_code"] {
		t.Fatal("legacy run_code tool still exists")
	}
}

func TestExecHTTPContractUsesScriptAndUnversionedPath(t *testing.T) {
	manager := newTaskManager(testConfig(t))
	body := `{"script":"printf api","mode":"shell","foregroundWaitMs":5000}`
	request := httptest.NewRequest(http.MethodPost, "/exec", strings.NewReader(body))
	response := httptest.NewRecorder()
	execHandler(manager).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", response.Code, response.Body.String())
	}
	var result taskView
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Stdout.Content != "api" {
		t.Fatalf("unexpected stdout: %#v", result.Stdout)
	}

	handler := newHTTPHandler(config{}, manager)
	legacy := httptest.NewRequest(http.MethodPost, "/v1/exec", strings.NewReader(`{"command":"true"}`))
	legacyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyResponse, legacy)
	if legacyResponse.Code != http.StatusNotFound {
		t.Fatalf("legacy versioned API is still routed: status %d", legacyResponse.Code)
	}
}

func TestAuthenticationRequired(t *testing.T) {
	handler := authenticated(config{token: "expected"}, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/exec", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", response.Code)
	}
}

func TestAuthenticationDisabledWithoutToken(t *testing.T) {
	handler := authenticated(config{}, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/exec", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d", response.Code)
	}
}

func TestTaskIDsAreNumericAndNotPIDs(t *testing.T) {
	manager := newTaskManager(testConfig(t))
	first := manager.nextID()
	second := manager.nextID()
	if !strings.HasPrefix(first, "t") || !strings.HasPrefix(second, "t") {
		t.Fatalf("unexpected ids %q %q", first, second)
	}
	firstNumber, err := strconv.ParseUint(strings.TrimPrefix(first, "t"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	secondNumber, err := strconv.ParseUint(strings.TrimPrefix(second, "t"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if secondNumber != firstNumber+1 {
		t.Fatalf("ids are not monotonic: %s then %s", first, second)
	}
}
