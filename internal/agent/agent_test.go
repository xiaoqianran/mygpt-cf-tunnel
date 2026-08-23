package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCommandEndpointIsRegisteredAndAuthenticated(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token"})
	req := httptest.NewRequest(http.MethodPost, "/v1/command/run", strings.NewReader(`{"command":"id"}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected authenticated command route, got status %d", rec.Code)
	}
}

func TestOnlyCommandActionIsExposed(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token"})
	for _, path := range []string{"/v1/repository/sync", "/v1/files/read", "/v1/git/commit-push", "/v1/github/release"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unexpected legacy route %s: status %d", path, rec.Code)
		}
	}
}

func TestDecodeJSONRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	for _, body := range []string{
		`{"command":"true","unexpected":true}`,
		`{"command":"true"} {"command":"false"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		rec := httptest.NewRecorder()
		var dst struct {
			Command string `json:"command"`
		}
		if err := decodeJSON(rec, req, &dst); err == nil {
			t.Fatalf("expected invalid JSON body to be rejected: %s", body)
		}
	}
}

func TestCappedBuffer(t *testing.T) {
	buf := &cappedBuffer{limit: 5}
	if n, err := buf.Write([]byte("abcdefgh")); err != nil || n != 8 {
		t.Fatalf("unexpected write result: n=%d err=%v", n, err)
	}
	text, truncated := buf.snapshot()
	if text != "defgh" || !truncated {
		t.Fatalf("unexpected capped output: %q truncated=%v", text, truncated)
	}
	_, _ = buf.Write([]byte("ij"))
	if buf.String() != "fghij" {
		t.Fatalf("capped output should retain the tail: %q", buf.String())
	}
}

func TestOpenAPIExposesUniversalNonConsequentialRunCommand(t *testing.T) {
	var spec struct {
		Info struct {
			Version     string `json:"version"`
			Description string `json:"description"`
		} `json:"info"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			OperationID     string `json:"operationId"`
			IsConsequential *bool  `json:"x-openai-isConsequential"`
			Description     string `json:"description"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Info.Version != openAPIContractVersion {
		t.Fatalf("OpenAPI version %q does not match frozen contract version %q", spec.Info.Version, openAPIContractVersion)
	}
	if spec.Components.Schemas == nil {
		t.Fatal("components.schemas must be an object, even when empty")
	}
	if len(spec.Info.Description) > 300 {
		t.Fatalf("OpenAPI info description exceeds Builder limit: %d", len(spec.Info.Description))
	}
	if len(spec.Paths) != 4 {
		t.Fatalf("expected four action paths, got %d", len(spec.Paths))
	}
	methods, ok := spec.Paths["/v1/command/run"]
	if !ok {
		t.Fatal("runCommand path missing from OpenAPI")
	}
	op, ok := methods["post"]
	if !ok || op.OperationID != "runCommand" {
		t.Fatalf("unexpected runCommand operation: %#v", op)
	}
	if op.IsConsequential == nil || *op.IsConsequential {
		t.Fatal("runCommand action must be explicitly non-consequential to avoid per-call confirmation prompts")
	}
	for path, operationID := range map[string]string{
		"/v1/command/start":            "startCommand",
		"/v1/command/jobs/{id}":        "getCommandJob",
		"/v1/command/jobs/{id}/cancel": "cancelCommandJob",
	} {
		methods := spec.Paths[path]
		var got string
		for _, candidate := range methods {
			got = candidate.OperationID
		}
		if got != operationID {
			t.Fatalf("unexpected operation for %s: %q", path, got)
		}
	}
	if len(op.Description) > 300 {
		t.Fatalf("runCommand description exceeds Builder limit: %d", len(op.Description))
	}
	for _, phrase := range []string{"install", "workflow", "not a capability boundary"} {
		combined := strings.ToLower(spec.Info.Description + " " + op.Description)
		if !strings.Contains(combined, phrase) {
			t.Fatalf("OpenAPI should communicate universal capability; missing %q", phrase)
		}
	}
}

func TestOpenAPIContractIsFrozen(t *testing.T) {
	const want = "66a83f4cff2ec59b2b49768235f47daa458e693e54b66e5d28a754b6bf718b42"
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(openAPISpec)))
	if got != want {
		t.Fatalf("OpenAPI contract changed: got %s want %s; review OPENAPI_CONTRACT.md before updating the lock", got, want)
	}
}

func TestHealthVersionMatchesOpenAPI(t *testing.T) {
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status: %d", rec.Code)
	}
	var health struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.Version != serviceVersion {
		t.Fatalf("health version %q does not match %q", health.Version, serviceVersion)
	}
}

func TestHostCommandRunsInRequestedDirectoryAndAcceptsStdin(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	root := t.TempDir()
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20000})
	result, err := s.runHostCommand(context.Background(), `printf '%s|' "$PWD"; cat`, root, "from-stdin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != root+"|from-stdin" {
		t.Fatalf("unexpected host command result: %#v", result)
	}
}

func TestHostCommandTimeoutKillsWorkflow(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 5 * time.Second, MaxCommandOutputChars: 20000})
	workdir := t.TempDir()
	started := time.Now()
	result, err := s.runHostCommand(context.Background(), "sleep 10", workdir, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.ExitCode == 0 {
		t.Fatalf("expected timeout result, got %#v", result)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("timeout did not terminate workflow promptly: %s", time.Since(started))
	}
}

func TestAsyncCommandCompletes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "printf done", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _, ok := s.getJob(id, 0)
		if !ok {
			t.Fatal("job disappeared")
		}
		if got.Status == "completed" {
			if got.ExitCode == nil || *got.ExitCode != 0 || got.Stdout != "done" {
				t.Fatalf("unexpected result: %#v", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not complete")
}

func TestAsyncCommandCanBeCancelled(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "sleep 10", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.cancelJob(id); !ok {
		t.Fatal("job not found")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _, _ := s.getJob(id, 0)
		if got.Status == "cancelled" {
			if got.ExitCode == nil || *got.ExitCode == 0 {
				t.Fatalf("unexpected cancelled result: %#v", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job was not cancelled")
}

func TestAsyncCommandExposesLiveOutput(t *testing.T) {
	s := NewServer(Config{CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{
		Command: "printf first; sleep 0.2; printf second >&2; sleep 0.2; printf third",
		Workdir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	seenRunningOutput := false
	for time.Now().Before(deadline) {
		job, _, ok := s.getJob(id, 0)
		if !ok {
			t.Fatal("job disappeared")
		}
		if job.Status == "running" && strings.Contains(job.Stdout, "first") {
			seenRunningOutput = true
			if job.ExitCode != nil {
				t.Fatalf("running job must not have an exit code: %#v", job)
			}
		}
		if job.Status == "completed" {
			if !seenRunningOutput {
				t.Fatal("job completed before live output was observable")
			}
			if job.Stdout != "firstthird" || !strings.Contains(job.Stderr, "second") || job.ExitCode == nil || *job.ExitCode != 0 {
				t.Fatalf("unexpected completed job: %#v", job)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not complete")
}

func TestExplicitTimeoutCannotExceedServerLimit(t *testing.T) {
	s := NewServer(Config{CommandTimeout: time.Second, MaxCommandOutputChars: 20000})
	_, err := s.runHostCommand(context.Background(), "printf done", t.TempDir(), "", 2)
	if err == nil || !strings.Contains(err.Error(), "exceeds server limit") {
		t.Fatalf("expected server timeout limit error, got %v", err)
	}
}

func TestExplicitTimeoutCanShortenServerLimit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := NewServer(Config{CommandTimeout: 10 * time.Second, MaxCommandOutputChars: 20000})
	result, err := s.runHostCommand(context.Background(), "sleep 2", t.TempDir(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.ExitCode == 0 {
		t.Fatalf("explicit shorter timeout was not enforced: %#v", result)
	}
}

func TestAsyncCommandHTTPFlow(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: time.Second, MaxCommandOutputChars: 20000})
	h := s.Handler()

	body, err := json.Marshal(commandInput{Command: "printf done", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/command/start", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status: %d body=%s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("invalid start response: %s", rec.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		req = httptest.NewRequest(http.MethodGet, "/v1/command/jobs/"+started.ID, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var job commandJobView
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatalf("invalid job response: %s", rec.Body.String())
		}
		switch job.Status {
		case "completed":
			return
		case "failed", "cancelled", "timed_out":
			t.Fatalf("job ended as %s: %s", job.Status, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not complete through HTTP API")
}

func TestCappedBufferTailPreservesUTF8(t *testing.T) {
	buf := &cappedBuffer{limit: 100}
	_, _ = buf.Write([]byte("abc你好🙂"))
	text, truncated := buf.tail(2)
	if text != "好🙂" || !truncated {
		t.Fatalf("unexpected UTF-8 tail: %q truncated=%v", text, truncated)
	}
}

func waitForJobStdout(t *testing.T, s *Server, id, want string) commandJobView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _, ok := s.getJob(id, 0)
		if !ok {
			t.Fatal("job disappeared")
		}
		if strings.Contains(job.Stdout, want) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job stdout never contained %q", want)
	return commandJobView{}
}

func TestCommandJobLongPollBroadcastsChanges(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "printf ready; sleep 0.2; printf wake; sleep 0.2", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	initial := waitForJobStdout(t, s, id, "ready")

	type response struct {
		code int
		job  commandJobView
	}
	responses := make(chan response, 2)
	for i := 0; i < 2; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/v1/command/jobs/"+id+"?after="+strconv.FormatUint(initial.Revision, 10)+"&wait_seconds=2", nil)
			req.Header.Set("Authorization", "Bearer test-token")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			var job commandJobView
			_ = json.Unmarshal(rec.Body.Bytes(), &job)
			responses <- response{code: rec.Code, job: job}
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case got := <-responses:
			if got.code != http.StatusOK || got.job.Revision == initial.Revision || !strings.Contains(got.job.Stdout, "wake") {
				t.Fatalf("waiter did not observe change: code=%d job=%#v", got.code, got.job)
			}
		case <-time.After(1500 * time.Millisecond):
			t.Fatal("long poll did not wake")
		}
	}
}

func TestCommandJobLongPollDisconnectDoesNotCancelJob(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "printf ready; sleep 0.3; printf done", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	initial := waitForJobStdout(t, s, id, "ready")

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/command/jobs/"+id+"?after="+strconv.FormatUint(initial.Revision, 10)+"&wait_seconds=2", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer test-token")
	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled HTTP waiter did not return")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _, _ := s.getJob(id, 0)
		if job.Status == "completed" {
			if job.Stdout != "readydone" {
				t.Fatalf("unexpected completed job: %#v", job)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("HTTP disconnect cancelled the background job")
}

func TestCommandJobLongPollHeartbeatAndValidation(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "printf ready; sleep 2", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	initial := waitForJobStdout(t, s, id, "ready")

	started := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/v1/command/jobs/"+id+"?after="+strconv.FormatUint(initial.Revision, 10)+"&wait_seconds=1", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("unexpected heartbeat wait: %s", elapsed)
	}
	var heartbeat commandJobView
	if err := json.Unmarshal(rec.Body.Bytes(), &heartbeat); err != nil {
		t.Fatal(err)
	}
	if heartbeat.Status != "running" || heartbeat.Revision != initial.Revision {
		t.Fatalf("unexpected heartbeat: %#v", heartbeat)
	}

	for _, query := range []string{"after=bad", "wait_seconds=1", "after=1&wait_seconds=21", "tail_chars=20001"} {
		req = httptest.NewRequest(http.MethodGet, "/v1/command/jobs/"+id+"?"+query, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: expected 400, got %d body=%s", query, rec.Code, rec.Body.String())
		}
	}
	_, _ = s.cancelJob(id)
}

func TestCommandJobLongPollOnlyWaitsOnMatchingRevision(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "printf ready; sleep 2", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	current := waitForJobStdout(t, s, id, "ready")

	for _, after := range []uint64{0, current.Revision + 1} {
		started := time.Now()
		req := httptest.NewRequest(http.MethodGet, "/v1/command/jobs/"+id+"?after="+strconv.FormatUint(after, 10)+"&wait_seconds=2", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("mismatched revision %d waited unexpectedly: %s", after, elapsed)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("mismatched revision %d: status %d body=%s", after, rec.Code, rec.Body.String())
		}
	}
	_, _ = s.cancelJob(id)
}

func TestOpenAPIExposesLongPollContract(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatal(err)
	}
	job := spec.Components.Schemas["CommandJob"]
	if _, ok := job.Properties["revision"]; !ok {
		t.Fatal("CommandJob.revision missing from OpenAPI")
	}
	params := map[string]bool{}
	for _, p := range spec.Paths["/v1/command/jobs/{id}"]["get"].Parameters {
		params[p.Name] = true
	}
	for _, name := range []string{"id", "after", "wait_seconds", "tail_chars"} {
		if !params[name] {
			t.Fatalf("getCommandJob parameter %q missing from OpenAPI", name)
		}
	}
}

func TestAsyncCommandNonZeroExitIsFailed(t *testing.T) {
	s := NewServer(Config{CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	id, err := s.startJob(commandInput{Command: "printf boom >&2; exit 7", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _, ok := s.getJob(id, 0)
		if !ok {
			t.Fatal("job disappeared")
		}
		if job.Status == "failed" {
			if job.ExitCode == nil || *job.ExitCode != 7 || !strings.Contains(job.Stderr, "boom") {
				t.Fatalf("unexpected failed job: %#v", job)
			}
			return
		}
		if job.Status != "running" {
			t.Fatalf("non-zero command ended as %s: %#v", job.Status, job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("non-zero command did not finish")
}

func TestTimeoutAboveServerLimitIsRejectedByHTTP(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: time.Second, MaxCommandOutputChars: 20000})
	for _, path := range []string{"/v1/command/run", "/v1/command/start"} {
		body := strings.NewReader(`{"command":"true","timeout_seconds":2}`)
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot exceed server limit") {
			t.Fatalf("%s: expected timeout limit 400, got %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestJobHistoryIsBoundedAndExpires(t *testing.T) {
	now := time.Now().UTC()
	store := newJobStore()
	running := &commandJob{ID: "running", Status: "running", StartedAt: now, changed: make(chan struct{})}
	store.jobs[running.ID] = running

	for i := 0; i < maxJobHistory+40; i++ {
		finished := now.Add(-time.Duration(i) * time.Minute)
		id := "job-" + strconv.Itoa(i)
		store.jobs[id] = &commandJob{ID: id, Status: "completed", FinishedAt: &finished, changed: make(chan struct{})}
	}
	expiredAt := now.Add(-jobRetention - time.Minute)
	store.jobs["expired"] = &commandJob{ID: "expired", Status: "completed", FinishedAt: &expiredAt, changed: make(chan struct{})}

	store.pruneLocked(now)
	if _, ok := store.jobs["running"]; !ok {
		t.Fatal("running job must never be evicted as history")
	}
	if _, ok := store.jobs["expired"]; ok {
		t.Fatal("expired job was not pruned")
	}
	terminal := 0
	for _, job := range store.jobs {
		if job.FinishedAt != nil {
			terminal++
		}
	}
	if terminal != maxJobHistory {
		t.Fatalf("terminal history=%d want=%d", terminal, maxJobHistory)
	}
	if _, ok := store.jobs["job-0"]; !ok {
		t.Fatal("newest terminal job should be retained")
	}
}

func TestNaturalExitResultIsNotReclassifiedAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewServer(Config{CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	result, err := s.runHostCommand(ctx, "true", t.TempDir(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if result.cancelled || result.TimedOut || result.ExitCode != 0 {
		t.Fatalf("natural exit was reclassified after cancellation: %#v", result)
	}
}

func TestLargeRunCommandReturnsCompleteActionFile(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	body, err := json.Marshal(commandInput{Command: `printf '%0200000d' 0`, Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/command/run", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() >= maxActionJSONBytes {
		t.Fatalf("action JSON is too large: %d bytes", rec.Body.Len())
	}

	var result commandActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Truncated || !result.InlineTruncated || !result.FullOutputAttached || result.CaptureTruncated {
		t.Fatalf("unexpected attachment state: %#v", result)
	}
	if len(result.OpenAIFileResponse) != 1 {
		t.Fatalf("attachment count=%d want=1: %#v", len(result.OpenAIFileResponse), result.OpenAIFileResponse)
	}
	if len(result.Stdout) > attachmentPreviewChars {
		t.Fatalf("inline preview too large: %d chars", len(result.Stdout))
	}

	u, err := url.Parse(result.OpenAIFileResponse[0])
	if err != nil {
		t.Fatal(err)
	}
	fileReq := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	fileRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(fileRec, fileReq)
	if fileRec.Code != http.StatusOK {
		t.Fatalf("file status=%d body=%s", fileRec.Code, fileRec.Body.String())
	}
	if fileRec.Body.Len() != 200000 {
		t.Fatalf("file bytes=%d want=200000", fileRec.Body.Len())
	}
	if !strings.HasPrefix(fileRec.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("missing attachment Content-Disposition: %q", fileRec.Header().Get("Content-Disposition"))
	}
	if strings.Trim(fileRec.Body.String(), "0") != "" {
		t.Fatal("attachment content differs from full stdout")
	}
}

func TestSmallRunCommandStaysInline(t *testing.T) {
	s := NewServer(Config{APIToken: "test-token", CommandTimeout: 3 * time.Second, MaxCommandOutputChars: 20000})
	body := strings.NewReader(`{"command":"printf hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/command/run", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result commandActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "hello" || result.Truncated || result.FullOutputAttached || len(result.OpenAIFileResponse) != 0 {
		t.Fatalf("small output should remain inline: %#v", result)
	}
}

func TestOpenAPIExposesActionFileResponse(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatal(err)
	}
	for _, schemaName := range []string{"CommandResult", "CommandJob"} {
		if _, ok := spec.Components.Schemas[schemaName].Properties["openaiFileResponse"]; !ok {
			t.Fatalf("%s.openaiFileResponse missing from OpenAPI", schemaName)
		}
	}
}
