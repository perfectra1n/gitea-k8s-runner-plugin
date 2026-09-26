//go:build linux

// Package stephelper is the step helper that runs inside job pods. The plugin
// execs `run` for each step: it starts a supervisor detached from the exec
// stream, which runs the step and records its output and exit code as a
// stepio log, then follows that log onto the stream. When the stream breaks,
// the step carries on; the plugin execs `follow` to pick the log up again
// from the last byte it received.
//
// A step directory holds:
//
//	log         the stepio frames, appended by the supervisor
//	supervisor  "pid starttime" of the supervisor, for liveness checks
//	done        created once the exit frame is written; its content, if any,
//	            is an error that kept the log from being complete
package stephelper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/stepio"
)

const (
	logFile        = "log"
	supervisorFile = "supervisor"
	doneFile       = "done"

	exitUsage          = stepio.ExitUsage
	exitSupervisorGone = stepio.ExitSupervisorGone
	exitNoLog          = stepio.ExitNoLog
	exitIncomplete     = stepio.ExitIncomplete

	readBuf = 32 << 10
)

var (
	// pollInterval is how often follow looks for new log data when idle.
	pollInterval = 50 * time.Millisecond
	// livenessEvery is how many idle polls pass between supervisor checks.
	livenessEvery = 20
	// drainGrace is how long output may keep arriving after the step's
	// process exits, from processes it left running that share its stdout.
	drainGrace = 2 * time.Second
)

// Main runs one helper command and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stderr, "usage: install DEST | run ... | follow DIR OFFSET")
		return exitUsage
	}
	switch args[0] {
	case "install":
		if len(args) != 2 {
			say(stderr, "usage: install DEST")
			return exitUsage
		}
		if err := Install(args[1]); err != nil {
			say(stderr, "%v", err)
			return 1
		}
		return 0
	case "run":
		spec, err := stepio.ParseRun(args[1:])
		if err != nil {
			say(stderr, "%v", err)
			return exitUsage
		}
		return run(spec, args[1:], stdout, stderr)
	case "supervise":
		// The spec arrives on stdin, never on the command line: see run.
		if len(args) != 2 {
			say(stderr, "usage: supervise DIR < SPEC")
			return exitUsage
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			say(stderr, "%v", err)
			return 1
		}
		spec, err := stepio.ParseRun(strings.Split(string(b), "\x00"))
		if err != nil || spec.Dir != args[1] {
			say(stderr, "supervise: bad spec: %v", err)
			return exitUsage
		}
		supervise(spec)
		return 0
	case "follow":
		dir, off, err := stepio.ParseFollow(args[1:])
		if err != nil {
			say(stderr, "%v", err)
			return exitUsage
		}
		return follow(dir, off, stdout, stderr)
	}
	say(stderr, "unknown command %q", args[0])
	return exitUsage
}

// Install copies the running binary to dest, atomically and executable by
// every user (the step user is not known here).
func Install(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self) //nolint:gosec // G304: our own binary
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // G301: the step user must traverse it
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".install-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //nolint:gosec // G703: a temp file we created next to dest
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest) //nolint:gosec // G703: dest is the install target given by the pod spec
}

// run creates the step directory, starts the supervisor and follows the log.
func run(spec stepio.RunSpec, args []string, stdout, stderr io.Writer) int {
	if err := os.MkdirAll(filepath.Dir(spec.Dir), 0o755); err != nil { //nolint:gosec // G301: shared by every step user
		say(stderr, "%v", err)
		return 1
	}
	collect(filepath.Dir(spec.Dir), filepath.Base(spec.Dir))
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil { //nolint:gosec // G301: see above
		say(stderr, "step directory: %v", err)
		return 1
	}
	// Creating the log is what starts the step, exactly once. A repeated run
	// (the plugin's first stream broke before any output arrived, while that
	// run may still be going) finds the log and follows it instead.
	f, err := os.OpenFile(filepath.Join(spec.Dir, logFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // G302: read back by follow
	if errors.Is(err, os.ErrExist) {
		return becomeFollower(spec.Dir, stdout, stderr)
	}
	if err != nil {
		say(stderr, "%v", err)
		return 1
	}
	_ = f.Close()

	self, err := os.Executable()
	if err != nil {
		say(stderr, "%v", err)
		return 1
	}
	// Its own session and no stdio of ours: nothing that happens to this
	// exec stream reaches it. The step's environment may hold secrets and
	// /proc/<pid>/cmdline is world-readable, so the spec goes over a pipe.
	specR, specW, err := os.Pipe()
	if err != nil {
		say(stderr, "%v", err)
		return 1
	}
	sup := exec.Command(self, "supervise", spec.Dir) //nolint:gosec // G204: re-exec of this binary
	sup.Stdin = specR
	sup.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = sup.Start()
	_ = specR.Close()
	if err != nil {
		_ = specW.Close()
		say(stderr, "start supervisor: %v", err)
		return 1
	}
	_, werr := specW.Write([]byte(strings.Join(args, "\x00")))
	if err := specW.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		say(stderr, "pass the step to its supervisor: %v", werr)
		return 1
	}
	pid := sup.Process.Pid
	if err := writeFile(filepath.Join(spec.Dir, supervisorFile), fmt.Sprintf("%d %d", pid, startTime(pid))); err != nil {
		say(stderr, "%v", err)
		return 1
	}
	if spec.Pidfile != "" {
		_ = os.MkdirAll(filepath.Dir(spec.Pidfile), 0o755) //nolint:gosec // G301: see above
		_ = writeFile(spec.Pidfile, strconv.Itoa(pid))
	}
	_ = sup.Process.Release()
	return becomeFollower(spec.Dir, stdout, stderr)
}

// becomeFollower turns run into `follow DIR 0` by exec'ing it, so the step's
// environment leaves this process's command line a moment after the exec
// that started it, as it did when steps ran under `env`. Tests follow
// in-process instead.
var becomeFollower = func(dir string, _, stderr io.Writer) int {
	self, err := os.Executable()
	if err == nil {
		argv := stepio.FollowArgv(self, dir, 0)
		err = syscall.Exec(self, argv, os.Environ()) //nolint:gosec // G204: re-exec of this binary
	}
	say(stderr, "exec follow: %v", err)
	return 1
}

// collect removes the other step directories in parent whose step is over.
// Steps of one environment run one at a time, so this never races a live one
// except by mistake, and a live supervisor is always left alone.
func collect(parent, keep string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep || !e.IsDir() {
			continue
		}
		dir := filepath.Join(parent, e.Name())
		if exists(filepath.Join(dir, doneFile)) || !supervisorAlive(dir) {
			_ = os.RemoveAll(dir)
		}
	}
}

// supervise runs the step, appending its output and then its exit code to
// the log, and marks the log done.
func supervise(spec stepio.RunSpec) {
	f, err := os.OpenFile(filepath.Join(spec.Dir, logFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		_ = writeFile(filepath.Join(spec.Dir, doneFile), "open log: "+err.Error())
		return
	}
	l := &frameLog{f: f}
	code := start(spec, l)
	l.finish(code)
	msg := ""
	if l.err != nil {
		msg = "write log: " + l.err.Error()
	}
	_ = writeFile(filepath.Join(spec.Dir, doneFile), msg)
	// Keep draining for processes the step left running (a server started
	// with &): closing their stdout would kill them on their next write.
	l.wait()
}

// start runs the command and returns its exit code, 126/127 like a shell
// when it cannot be started. Output goes to l.
func start(spec stepio.RunSpec, l *frameLog) int {
	fail := func(code int, format string, a ...any) int {
		l.frame(stepio.KindStderr, []byte(fmt.Sprintf(format, a...)+"\n"))
		return code
	}
	if spec.Workdir != "" {
		if err := os.MkdirAll(spec.Workdir, 0o755); err != nil { //nolint:gosec // G301: a workspace directory
			return fail(1, "workdir %s: %v", spec.Workdir, err)
		}
		if err := os.Chdir(spec.Workdir); err != nil {
			return fail(1, "workdir %s: %v", spec.Workdir, err)
		}
	}
	env := mergeEnv(os.Environ(), spec.Env)
	bin, err := lookPath(spec.Cmd[0], env)
	if err != nil {
		return fail(127, "%s: %v", spec.Cmd[0], err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(1, "pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return fail(1, "pipe: %v", err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return fail(1, "%v", err)
	}
	p, err := os.StartProcess(bin, spec.Cmd, &os.ProcAttr{Env: env, Files: []*os.File{devnull, outW, errW}})
	_ = outW.Close()
	_ = errW.Close()
	_ = devnull.Close()
	if err != nil {
		_ = outR.Close()
		_ = errR.Close()
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ENOEXEC) {
			return fail(126, "%s: %v", spec.Cmd[0], err)
		}
		return fail(127, "%s: %v", spec.Cmd[0], err)
	}
	l.copy(stepio.KindStdout, outR)
	l.copy(stepio.KindStderr, errR)
	st, err := p.Wait()
	if err != nil {
		return fail(1, "wait: %v", err)
	}
	l.settle(drainGrace)
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return st.ExitCode()
}

// frameLog appends frames to the log from several readers. After finish it
// keeps reading them but discards what they produce.
type frameLog struct {
	f      *os.File
	mu     sync.Mutex
	closed bool
	err    error
	wg     sync.WaitGroup
}

func (l *frameLog) frame(kind byte, p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.err != nil {
		return
	}
	_, l.err = l.f.Write(stepio.AppendFrame(nil, kind, p))
}

func (l *frameLog) copy(kind byte, r *os.File) {
	l.wg.Go(func() {
		defer func() { _ = r.Close() }()
		buf := make([]byte, readBuf)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				l.frame(kind, buf[:n])
			}
			if err != nil {
				return
			}
		}
	})
}

// settle waits for every reader to reach EOF, for at most grace.
func (l *frameLog) settle(grace time.Duration) {
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

func (l *frameLog) finish(code int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		_, l.err = l.f.Write(stepio.ExitFrame(code))
	}
	if err := l.f.Close(); l.err == nil {
		l.err = err
	}
	l.closed = true
}

func (l *frameLog) wait() { l.wg.Wait() }

// follow copies the log from off to stdout until it is complete.
func follow(dir string, off int64, stdout, stderr io.Writer) int {
	f, err := os.Open(filepath.Join(dir, logFile)) //nolint:gosec // G304: a step directory named by the plugin
	if err != nil {
		say(stderr, "step log: %v", err)
		return exitNoLog
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || fi.Size() < off {
		say(stderr, "step log is shorter than offset %d", off)
		return exitNoLog
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		say(stderr, "%v", err)
		return exitNoLog
	}
	buf := make([]byte, readBuf)
	drain := func() error {
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := stdout.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	for idle := 0; ; idle++ {
		if err := drain(); err != nil {
			say(stderr, "%v", err)
			return 1
		}
		// Read done before the final drain: everything logged before it was
		// created is then guaranteed to be read.
		if msg, ok := readDone(dir); ok {
			if err := drain(); err != nil {
				say(stderr, "%v", err)
				return 1
			}
			if msg != "" {
				say(stderr, "%v", msg)
				return exitIncomplete
			}
			return 0
		}
		if idle%livenessEvery == livenessEvery-1 && !supervisorAlive(dir) {
			if _, ok := readDone(dir); ok {
				continue // finished between the two checks
			}
			_ = drain()
			say(stderr, "the step's supervisor process is gone (main container restarted or the process was killed)")
			return exitSupervisorGone
		}
		time.Sleep(pollInterval)
	}
}

// say writes one line of diagnostics for the plugin to report.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", a...)
}

func readDone(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, doneFile)) //nolint:gosec // G304: our step directory
	if err != nil {
		return "", false
	}
	return string(b), true
}

// supervisorAlive reports whether the process recorded in dir still runs.
// The start time guards against the pid being reused after a restart.
func supervisorAlive(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, supervisorFile)) //nolint:gosec // G304: our step directory
	if err != nil {
		return false
	}
	var pid int
	var st uint64
	if _, err := fmt.Sscanf(string(b), "%d %d", &pid, &st); err != nil {
		return false
	}
	state, start, ok := procStat(pid)
	return ok && start == st && state != 'Z' && state != 'X'
}

func startTime(pid int) uint64 {
	_, st, _ := procStat(pid)
	return st
}

// procStat returns a process's state and start time from /proc/<pid>/stat.
func procStat(pid int) (byte, uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, false
	}
	// The command name may contain spaces and parentheses; fields resume
	// after the last ')'. State is field 3, starttime field 22.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 20 {
		return 0, 0, false
	}
	st, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], st, true
}

// mergeEnv layers NAME=value assignments over base, later ones winning.
func mergeEnv(base, over []string) []string {
	idx := map[string]int{}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range append(append([]string(nil), base...), over...) {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[k]; ok {
			out[i] = kv
			continue
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	return out
}

// lookPath resolves name like env(1): through PATH from the step's own
// environment, not the helper's.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathVar := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	for _, d := range filepath.SplitList(pathVar) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// writeFile writes content to p atomically.
func writeFile(p, content string) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil { //nolint:gosec // G306: read by follow and the plugin's kill
		return err
	}
	return os.Rename(tmp, p)
}
