package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/perfectra1n/gitea-k8s-runner-plugin/gen/plugin/v1alpha"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/k8s"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/podspec"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/stepio"
)

// stepLog is a helper step's complete log.
func stepLog(code int, chunks ...string) []byte {
	var b []byte
	for i, c := range chunks {
		kind := stepio.KindStdout
		if i%2 == 1 {
			kind = stepio.KindStderr
		}
		b = stepio.AppendFrame(b, kind, []byte(c))
	}
	return append(b, stepio.ExitFrame(code)...)
}

// fakeHelper plays the step helper in the pod: each helper exec is answered
// by the next scripted attempt, which serves log[offset:] (the whole log for
// `run`) up to cut bytes and then returns err.
type fakeHelper struct {
	mu       sync.Mutex
	log      []byte
	attempts []attempt
	seen     [][]string
}

type attempt struct {
	cut      int // bytes to serve; -1 for all
	err      error
	code     int
	stderr   string
	noOutput bool
}

func (h *fakeHelper) exec(_ context.Context, cmd []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
	if cmd[0] != podspec.HelperPath {
		return 0, nil // prepare, env, kill
	}
	h.mu.Lock()
	h.seen = append(h.seen, cmd)
	if len(h.attempts) == 0 {
		h.mu.Unlock()
		return -1, errors.New("unscripted helper exec")
	}
	a := h.attempts[0]
	h.attempts = h.attempts[1:]
	h.mu.Unlock()
	off := 0
	if cmd[1] == "follow" {
		n, err := strconv.Atoi(cmd[3])
		if err != nil {
			return -1, err
		}
		off = n
	}
	if !a.noOutput {
		rest := h.log[off:]
		if a.cut >= 0 && a.cut < len(rest) {
			rest = rest[:a.cut]
		}
		if _, err := stdout.Write(rest); err != nil {
			return -1, err
		}
	}
	_, _ = io.WriteString(stderr, a.stderr)
	return a.code, a.err
}

func (h *fakeHelper) calls() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.seen...)
}

var errReset = errors.New("exec in pod p: read tcp 10.0.0.1:1->10.96.0.1:443: read: connection reset by peer (pod phase=Running)")

func helperServer(t *testing.T, h *fakeHelper, timeout time.Duration) (pluginv1.BackendPluginClient, *fakeBackend, string) {
	t.Helper()
	fb := &fakeBackend{exec: h.exec}
	c, _ := setupWith(t, fb, func(s *Server) {
		s.defaults.StepHelperImage = "ghcr.io/x/plugin:1"
		s.ReattachTimeout = timeout
		s.reattachBackoff = time.Millisecond
	})
	id := create(t, c, nil).GetEnvironmentId()
	if _, _, err := start(t, c, id); err != nil {
		t.Fatal(err)
	}
	return c, fb, id
}

func TestExecResumesABrokenStream(t *testing.T) {
	log := stepLog(3, "hello ", "warn\n", "world\n")
	// Broken inside the second frame's header, then inside the third
	// frame's payload, then complete.
	h := &fakeHelper{log: log, attempts: []attempt{
		{cut: 13, err: errReset},
		{cut: 14, err: errReset},
		{cut: -1},
	}}
	c, fb, id := helperServer(t, h, time.Minute)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"make", "test"},
		Env: map[string]string{"A": "1"}, Workdir: "/__w/o/r"})
	if r.complete == nil || r.complete.GetExitCode() != 3 || r.failed != nil {
		t.Fatalf("result: %+v", r)
	}
	if r.stdout != "hello world\n" || r.stderr != "warn\n" {
		t.Fatalf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
	calls := h.calls()
	if len(calls) != 3 {
		t.Fatalf("helper execs = %q", calls)
	}
	run := calls[0]
	dir := run[2]
	if run[1] != "run" || !strings.HasPrefix(dir, stepDir+"/") || strings.Join(run[5:], " ") != "A=1 -- make test" || run[4] != "/__w/o/r" {
		t.Errorf("run argv = %q", run)
	}
	if want := stepio.FollowArgv(podspec.HelperPath, dir, 13); strings.Join(calls[1], " ") != strings.Join(want, " ") {
		t.Errorf("first follow = %q, want %q", calls[1], want)
	}
	if want := stepio.FollowArgv(podspec.HelperPath, dir, 27); strings.Join(calls[2], " ") != strings.Join(want, " ") {
		t.Errorf("second follow = %q, want %q", calls[2], want)
	}
	job := fb.created[0].Spec.Template.Spec
	if job.InitContainers[0].Name != podspec.HelperContainer || job.InitContainers[0].Image != "ghcr.io/x/plugin:1" {
		t.Errorf("job lacks the step helper: %v", job.InitContainers)
	}
}

// While the API server is unreachable, reattaching keeps failing before any
// output; that is retried until it works.
func TestExecRetriesFailedReattaches(t *testing.T) {
	h := &fakeHelper{log: stepLog(0, "ok\n"), attempts: []attempt{
		{cut: 0, err: errReset},
		{noOutput: true, err: errors.New("dial tcp 10.96.0.1:443: connect: connection refused")},
		{noOutput: true, err: errors.New("dial tcp 10.96.0.1:443: connect: connection refused")},
		{cut: -1},
	}}
	c, _, id := helperServer(t, h, time.Minute)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	if r.complete == nil || r.complete.GetExitCode() != 0 || r.stdout != "ok\n" {
		t.Fatalf("result: %+v", r)
	}
}

func TestExecGivesUpAfterReattachTimeout(t *testing.T) {
	var attempts []attempt
	for range 1000 {
		attempts = append(attempts, attempt{noOutput: true, err: errReset})
	}
	h := &fakeHelper{log: stepLog(0), attempts: attempts}
	c, _, id := helperServer(t, h, 50*time.Millisecond)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	if r.failed == nil || !strings.Contains(r.failed.GetErrorMessage(), "connection reset") ||
		!strings.Contains(r.failed.GetErrorMessage(), "resum") {
		t.Fatalf("result: %+v", r)
	}
}

func TestExecDoesNotResumeAFinishedPod(t *testing.T) {
	gone := fmt.Errorf("exec in pod p: EOF (the pod no longer exists: deleted or evicted): %w", k8s.ErrPodFinished)
	h := &fakeHelper{log: stepLog(0, "partial"), attempts: []attempt{{cut: 3, err: gone}}}
	c, _, id := helperServer(t, h, time.Minute)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	if r.failed == nil || !strings.Contains(r.failed.GetErrorMessage(), "no longer exists") {
		t.Fatalf("result: %+v", r)
	}
	if n := len(h.calls()); n != 1 {
		t.Fatalf("helper execs = %d, want 1", n)
	}
}

// Until output arrives the step may not have started, so reattaching runs it
// again (the helper's run follows a step that already started).
func TestExecReattachesWithRunUntilOutputArrives(t *testing.T) {
	h := &fakeHelper{log: stepLog(0, "ran\n"), attempts: []attempt{
		{noOutput: true, err: errReset},
		{cut: -1},
	}}
	c, _, id := helperServer(t, h, time.Minute)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	if r.complete == nil || r.stdout != "ran\n" {
		t.Fatalf("result: %+v", r)
	}
	calls := h.calls()
	if len(calls) != 2 || strings.Join(calls[1], " ") != strings.Join(calls[0], " ") {
		t.Fatalf("helper execs = %q", calls)
	}
}

func TestExecReportsAVanishedStep(t *testing.T) {
	h := &fakeHelper{log: stepLog(0, "partial output"), attempts: []attempt{
		{cut: 8, err: errReset},
		{noOutput: true, code: stepio.ExitSupervisorGone, stderr: "the step's supervisor process is gone (main container restarted or the process was killed)"},
	}}
	c, _, id := helperServer(t, h, time.Minute)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	if r.failed == nil || !strings.Contains(r.failed.GetErrorMessage(), "supervisor process is gone") {
		t.Fatalf("result: %+v", r)
	}
}

func TestExecRejectsACorruptLog(t *testing.T) {
	h := &fakeHelper{log: []byte{9, 0, 0, 0, 0}, attempts: []attempt{{cut: -1}}}
	c, _, id := helperServer(t, h, time.Minute)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	if r.failed == nil || !strings.Contains(r.failed.GetErrorMessage(), "corrupt") {
		t.Fatalf("result: %+v", r)
	}
}

// Cancelling kills the step through the pidfile the helper was given.
func TestExecCancelKillsHelperStep(t *testing.T) {
	running := make(chan struct{})
	fb := &fakeBackend{}
	fb.exec = func(ctx context.Context, cmd []string, _ io.Reader, _, _ io.Writer) (int, error) {
		if cmd[0] == podspec.HelperPath {
			close(running)
			<-ctx.Done()
			return -1, ctx.Err()
		}
		return 0, nil
	}
	c, _ := setupWith(t, fb, func(s *Server) { s.defaults.StepHelperImage = "img" })
	id := create(t, c, nil).GetEnvironmentId()
	if _, _, err := start(t, c, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.Exec(ctx, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"sleep", "1000"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = st.Recv() }()
	<-running
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		var pidfile string
		var killed bool
		for _, cmd := range fb.execCalls() {
			if cmd[0] == podspec.HelperPath {
				pidfile = cmd[3]
			}
			if strings.Contains(strings.Join(cmd, " "), "kill -TERM") && pidfile != "" && cmd[len(cmd)-2] == pidfile {
				killed = true
			}
		}
		if killed {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no kill of the helper's pidfile; execs = %q", fb.execCalls())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
