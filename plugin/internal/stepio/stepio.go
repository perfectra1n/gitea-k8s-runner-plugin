// Package stepio is the contract between the plugin and the step helper
// binary in the job pod: the argv of its `run` and `follow` commands, and the
// framed log a step's output and exit code are recorded in.
//
// The helper runs a step detached from the exec stream and appends frames to a
// log file; `follow` copies that file, raw, from a byte offset. The plugin
// decodes the frames and counts the raw bytes it received, so when an exec
// stream breaks it resumes at exactly that offset: nothing is lost or
// repeated, even when the break falls inside a frame. A step has ended only
// once its exit frame arrives, never because a stream closed.
package stepio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Frame kinds. A frame is kind (1 byte), payload length (4 bytes, big
// endian), payload.
const (
	KindStdout byte = 1
	KindStderr byte = 2
	// KindExit ends the log; its payload is the exit code as a big-endian
	// int32.
	KindExit byte = 3

	headerLen = 5
	// MaxPayload bounds a frame; the helper writes at most its read buffer.
	MaxPayload = 1 << 20
)

// Exit codes of the helper's `follow` (and of `run`, which ends by following)
// when it could not deliver a complete log. 0 means the log is complete.
const (
	ExitUsage = 2
	// ExitSupervisorGone: the step's supervisor died without finishing the
	// log (main restarted, or the process was killed).
	ExitSupervisorGone = 3
	// ExitNoLog: there is no log to follow, so the step never started.
	ExitNoLog = 4
	// ExitIncomplete: the log could not be written in full.
	ExitIncomplete = 5
)

// ErrCorrupt means the bytes are not a frame stream.
var ErrCorrupt = errors.New("corrupt step log")

// AppendFrame appends one frame to dst.
func AppendFrame(dst []byte, kind byte, payload []byte) []byte {
	var h [headerLen]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload))) //nolint:gosec // G115: payloads are bounded by MaxPayload
	return append(append(dst, h[:]...), payload...)
}

// ExitFrame returns the frame that ends a log with exit code code.
func ExitFrame(code int) []byte {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], uint32(int32(code))) //nolint:gosec // G115: exit codes fit in int32
	return AppendFrame(nil, KindExit, p[:])
}

// Decoder turns a frame stream written to it, in any chunking, into output on
// Stdout and Stderr plus an exit code.
type Decoder struct {
	Stdout, Stderr io.Writer

	offset  int64
	pending []byte // an incomplete frame
	code    int
	exited  bool
}

// Write consumes raw log bytes. A frame's payload is written out once the
// whole frame has arrived.
func (d *Decoder) Write(p []byte) (int, error) {
	d.offset += int64(len(p))
	d.pending = append(d.pending, p...)
	for len(d.pending) >= headerLen {
		if d.exited {
			return len(p), fmt.Errorf("%w: data after the exit record", ErrCorrupt)
		}
		kind, n := d.pending[0], binary.BigEndian.Uint32(d.pending[1:headerLen])
		switch {
		case n > MaxPayload:
			return len(p), fmt.Errorf("%w: frame of %d bytes", ErrCorrupt, n)
		case kind == KindExit && n != 4:
			return len(p), fmt.Errorf("%w: exit record of %d bytes", ErrCorrupt, n)
		case kind != KindStdout && kind != KindStderr && kind != KindExit:
			return len(p), fmt.Errorf("%w: frame kind %d", ErrCorrupt, kind)
		}
		end := headerLen + int(n)
		if len(d.pending) < end {
			break
		}
		payload := d.pending[headerLen:end]
		var err error
		switch kind {
		case KindStdout:
			_, err = d.Stdout.Write(payload)
		case KindStderr:
			_, err = d.Stderr.Write(payload)
		case KindExit:
			d.code, d.exited = int(int32(binary.BigEndian.Uint32(payload))), true //nolint:gosec // G115: round-trips ExitFrame
		}
		d.pending = d.pending[end:]
		if err != nil {
			return len(p), err
		}
	}
	if len(d.pending) == 0 {
		d.pending = nil // let a large drained buffer go
	}
	return len(p), nil
}

// Offset is how many raw bytes have been consumed: where to resume.
func (d *Decoder) Offset() int64 { return d.offset }

// Exit returns the exit code once the exit record has been decoded.
func (d *Decoder) Exit() (int, bool) { return d.code, d.exited }

// RunSpec is a parsed `run` command line.
type RunSpec struct {
	// Dir holds the step's log and state; created by run.
	Dir string
	// Pidfile receives the supervisor's pid, so a cancelled step's whole
	// process tree can be killed; empty for none.
	Pidfile string
	// Workdir is entered (created if missing) before the command starts;
	// empty keeps the helper's.
	Workdir string
	// Env are NAME=value assignments layered over the helper's environment.
	Env []string
	Cmd []string
}

// RunArgv returns the argv that starts cmd under the helper and follows its
// log from the start. Invalid env names are dropped, as env(1) would
// misread them.
func RunArgv(helper, dir, pidfile, workdir string, env map[string]string, cmd []string) []string {
	argv := []string{helper, "run", dir, pidfile, workdir}
	argv = append(argv, EnvArgs(env)...)
	argv = append(argv, "--")
	return append(argv, cmd...)
}

// ParseRun parses the arguments after `run`.
func ParseRun(args []string) (RunSpec, error) {
	if len(args) < 3 {
		return RunSpec{}, errors.New("usage: run DIR PIDFILE WORKDIR [NAME=value]... -- COMMAND [ARG]")
	}
	s := RunSpec{Dir: args[0], Pidfile: args[1], Workdir: args[2]}
	if !path.IsAbs(s.Dir) {
		return RunSpec{}, fmt.Errorf("run: DIR %q must be absolute", s.Dir)
	}
	rest := args[3:]
	for i, a := range rest {
		if a == "--" {
			s.Cmd = rest[i+1:]
			if len(s.Cmd) == 0 {
				return RunSpec{}, errors.New("run: no command after --")
			}
			return s, nil
		}
		if k, _, ok := strings.Cut(a, "="); !ok || !validName(k) {
			return RunSpec{}, fmt.Errorf("run: %q is not NAME=value", a)
		}
		s.Env = append(s.Env, a)
	}
	return RunSpec{}, errors.New("run: missing -- before the command")
}

// FollowArgv returns the argv that copies the log in dir from offset.
func FollowArgv(helper, dir string, offset int64) []string {
	return []string{helper, "follow", dir, strconv.FormatInt(offset, 10)}
}

// ParseFollow parses the arguments after `follow`.
func ParseFollow(args []string) (string, int64, error) {
	if len(args) != 2 || !path.IsAbs(args[0]) {
		return "", 0, errors.New("usage: follow DIR OFFSET")
	}
	off, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || off < 0 {
		return "", 0, fmt.Errorf("follow: bad offset %q", args[1])
	}
	return args[0], off, nil
}

// EnvArgs returns env as sorted NAME=value arguments, dropping names env(1)
// would not treat as an assignment.
func EnvArgs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if validName(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func validName(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00")
}
