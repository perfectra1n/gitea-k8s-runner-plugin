// Package server implements the plugin.v1alpha BackendPlugin gRPC service on
// top of a k8s.Backend: one Job per environment, steps and file copies over
// pods/exec into the `main` container.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	pluginv1 "github.com/perfectra1n/gitea-k8s-runner-plugin/gen/plugin/v1alpha"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/execwrap"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/k8s"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/options"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/podspec"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/tarx"
)

// Filesystem layout reported to the runner. Everything lives on the shared
// volume so a class can size it (emptyDir limit or an ephemeral PVC). The one
// exception is the tool cache: when the job image names its own (see
// prepareScript), toolCachePath becomes a symlink to it at Start.
//
// defaultPATH only matters when neither the job nor the image sets PATH: the
// runner falls back to it after the image's PATH from StartComplete.image_env.
const (
	rootPath      = podspec.SharedMount
	actPath       = rootPath + "/_act"
	toolCachePath = rootPath + "/_tool"
	tempPath      = rootPath + "/_temp"
	pidDir        = rootPath + "/.gitea-k8s-runner-plugin"
	defaultPATH   = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	backendName = "kubernetes"
	// killGrace is how long a cancelled command gets between SIGTERM and SIGKILL.
	killGrace = 5
	// stderrKeep bounds how much stderr of a copy command is kept for errors.
	stderrKeep = 4096
)

// Server is the BackendPlugin implementation.
type Server struct {
	pluginv1.UnimplementedBackendPluginServer

	backend     k8s.Backend
	defaults    options.Defaults
	loadPodspec func(string) (*corev1.PodSpec, error)

	mu   sync.Mutex
	envs map[string]*environment
	seq  atomic.Uint64
}

// environment is one workflow job's pod. lock serialises RPCs per
// environment (a cancelled stream may still be unwinding when Remove lands).
type environment struct {
	id   string
	opts options.Options
	in   podspec.BuildInput
	lock chan struct{}
	pod  string // set once Start succeeded
}

// New returns a server using backend, with defaults for unset options.
func New(backend k8s.Backend, defaults options.Defaults) *Server {
	return &Server{
		backend:     backend,
		defaults:    defaults,
		loadPodspec: podspec.Load,
		envs:        map[string]*environment{},
	}
}

func (s *Server) Capabilities(context.Context, *pluginv1.CapabilitiesRequest) (*pluginv1.CapabilitiesResponse, error) {
	return &pluginv1.CapabilitiesResponse{Name: backendName}, nil
}

func (s *Server) Create(_ context.Context, req *pluginv1.CreateRequest) (*pluginv1.CreateResponse, error) {
	opts, err := options.Parse(req.GetBackendOptions(), s.defaults)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	specPath := opts.Podspec
	if req.GetLabelArg() != "" {
		specPath = req.GetLabelArg()
	}
	if specPath == "" {
		return nil, status.Error(codes.InvalidArgument, "no podspec: set the plugin's podspec option or use a <name>:<plugin>:<podspec path> label")
	}
	spec, err := s.loadPodspec(specPath)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	id := newEnvID(req.GetName())
	services := make([]podspec.Service, 0, len(req.GetServices()))
	for _, sv := range req.GetServices() {
		services = append(services, podspec.Service{Name: sv.GetName(), Image: sv.GetImage(), Env: sv.GetEnv(), Ports: sv.GetPorts()})
	}
	in := podspec.BuildInput{
		Spec:             spec,
		EnvID:            id,
		Instance:         opts.Instance,
		Namespace:        opts.Namespace,
		Image:            req.GetImage(),
		Services:         services,
		Timeout:          req.GetEnvironmentTimeout().AsDuration(),
		Labels:           opts.Labels,
		PullPolicy:       opts.PullPolicy,
		ServiceResources: opts.ServiceResources,
	}
	// Validate now so a bad podspec/service fails Create, not Start.
	if _, err := podspec.BuildJob(in); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	s.mu.Lock()
	s.envs[id] = &environment{id: id, opts: opts, in: in, lock: make(chan struct{}, 1)}
	s.mu.Unlock()

	return &pluginv1.CreateResponse{
		EnvironmentId:       id,
		RootPath:            rootPath,
		ActPath:             actPath,
		ToolCachePath:       toolCachePath,
		TempPath:            tempPath,
		PathVariableName:    ptrTo("PATH"),
		DefaultPathVariable: ptrTo(defaultPATH),
		PathSeparator:       ptrTo(":"),
		Os:                  "Linux",
		Arch:                runnerArch(spec.NodeSelector["kubernetes.io/arch"]),
	}, nil
}

func (s *Server) Start(req *pluginv1.StartRequest, stream pluginv1.BackendPlugin_StartServer) error {
	ctx := stream.Context()
	e, unlock, err := s.acquire(ctx, req.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer unlock()
	if e.pod != "" {
		return status.Errorf(codes.FailedPrecondition, "environment %s already started", e.id)
	}

	pod, imageEnv, err := s.boot(ctx, e, func(line string) {
		_ = stream.Send(&pluginv1.StartOutput{Output: &pluginv1.StartOutput_Data{Data: &pluginv1.DataChunk{
			Stream: pluginv1.DataChunk_STDOUT, Data: []byte(line + "\n"),
		}}})
	})
	if err != nil {
		// Don't leave a half-started pod holding capacity; Remove may never
		// come if the runner gives up on this job.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = s.backend.Delete(dctx, e.id)
		return err
	}
	e.pod = pod
	return stream.Send(&pluginv1.StartOutput{Output: &pluginv1.StartOutput_StartComplete{
		StartComplete: &pluginv1.StartComplete{ImageEnv: imageEnv},
	}})
}

func (s *Server) boot(ctx context.Context, e *environment, progress func(string)) (string, map[string]string, error) {
	job, err := podspec.BuildJob(e.in)
	if err != nil {
		return "", nil, status.Error(codes.InvalidArgument, err.Error())
	}
	rctx, cancel := context.WithTimeout(ctx, e.opts.ReadyTimeout)
	defer cancel()
	if err := s.backend.Create(rctx, job); err != nil {
		return "", nil, statusFromCtx(rctx, err, codes.FailedPrecondition)
	}
	progress(fmt.Sprintf("created job %s/%s", job.Namespace, job.Name))
	pod, err := s.backend.WaitReady(rctx, e.id, progress)
	if err != nil {
		return "", nil, statusFromCtx(rctx, err, codes.FailedPrecondition)
	}
	toolCache, err := s.prepare(rctx, pod)
	if err != nil {
		return "", nil, statusFromCtx(rctx, err, codes.FailedPrecondition)
	}
	if toolCache != toolCachePath {
		progress(fmt.Sprintf("tool cache %s -> %s (from the job image)", toolCachePath, toolCache))
	}
	progress(fmt.Sprintf("pod %s is ready", pod))
	return pod, s.imageEnv(rctx, pod), nil
}

// prepareScript creates the layout directories and resolves the tool cache.
// $1 is toolCachePath, the rest are directories to create.
//
// The runner derives RUNNER_TOOL_CACHE and runner.tool_cache from
// CreateResponse, which is answered before the pod exists and overrides
// whatever the image sets. So instead of reporting the image's tool cache,
// toolCachePath is made a symlink to it: the image's RUNNER_TOOL_CACHE, else
// AGENT_TOOLSDIRECTORY (both from main's env, i.e. image ENV plus podspec
// env). Tools baked into the image are then found by actions/setup-* instead
// of being downloaded every job. The symlink is only made when that directory
// is absolute, writable by the step user (a cache miss must be able to add a
// version) and toolCachePath is not already there (e.g. a mounted volume);
// otherwise toolCachePath stays a plain directory on the shared volume. The
// path in use is printed on stdout. Idempotent, as prepare may retry it.
const prepareScript = `d=$1; shift
mkdir -p "$@" || exit
t=${RUNNER_TOOL_CACHE:-${AGENT_TOOLSDIRECTORY:-$d}}
case $t in
/?*)
  if [ "$t" != "$d" ] && [ ! -e "$d" ] && [ ! -L "$d" ] &&
    mkdir -p "$t" 2>/dev/null && [ -d "$t" ] && [ -w "$t" ] && ln -s "$t" "$d" 2>/dev/null; then
    echo "$t"
    exit 0
  fi
  ;;
esac
if [ -L "$d" ]; then
  readlink "$d" || echo "$d"
  exit 0
fi
mkdir -p "$d" && echo "$d"`

// prepare creates the layout directories and returns the directory the tool
// cache resolves to. It doubles as the exec probe: the API server may accept
// exec a moment after the pod reports Ready.
func (s *Server) prepare(ctx context.Context, pod string) (string, error) {
	cmd := []string{"/bin/sh", "-c", prepareScript, "sh", toolCachePath, actPath, tempPath}
	var lastErr error
	for attempt := 0; ; attempt++ {
		var stdout, stderr bytes.Buffer
		code, err := s.backend.Exec(ctx, pod, cmd, nil, &stdout, &stderr)
		switch {
		case err == nil && code == 0:
			return resolvedToolCache(stdout.String()), nil
		case err == nil:
			return "", fmt.Errorf("creating %s in main failed (exit %d): %s", rootPath, code, strings.TrimSpace(stderr.String()))
		case missingBinary(err):
			return "", fmt.Errorf("main container needs /bin/sh (plus env and tar): %w", err)
		}
		lastErr = err
		if attempt >= 10 {
			return "", fmt.Errorf("exec into main does not work: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("exec into main does not work: %w", lastErr)
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
}

// resolvedToolCache reads prepareScript's output: the last line, if it is an
// absolute path, else toolCachePath.
func resolvedToolCache(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if p := strings.TrimSpace(lines[len(lines)-1]); path.IsAbs(p) {
		return p
	}
	return toolCachePath
}

func missingBinary(err error) bool {
	m := err.Error()
	return strings.Contains(m, "no such file or directory") || strings.Contains(m, "executable file not found")
}

// imageEnv reads main's environment: the image's ENV plus podspec env, which
// the runner layers beneath the job's own variables. Best effort.
func (s *Server) imageEnv(ctx context.Context, pod string) map[string]string {
	var out bytes.Buffer
	sep := "\x00"
	if code, err := s.backend.Exec(ctx, pod, []string{"env", "-0"}, nil, &out, io.Discard); err != nil || code != 0 {
		out.Reset()
		sep = "\n"
		if code, err := s.backend.Exec(ctx, pod, []string{"env"}, nil, &out, io.Discard); err != nil || code != 0 {
			return nil
		}
	}
	env := map[string]string{}
	for _, kv := range strings.Split(out.String(), sep) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		switch k {
		case "HOSTNAME", "PWD", "SHLVL", "_", "HOME":
			// Per-pod or per-shell values; HOME is re-derived by the runner.
			continue
		}
		env[k] = v
	}
	return env
}

func (s *Server) Exec(req *pluginv1.ExecRequest, stream pluginv1.BackendPlugin_ExecServer) error {
	ctx := stream.Context()
	e, unlock, err := s.acquire(ctx, req.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer unlock()
	if e.pod == "" {
		return status.Errorf(codes.FailedPrecondition, "environment %s is not started", e.id)
	}
	fail := func(msg string) error {
		return stream.Send(&pluginv1.ExecOutput{Output: &pluginv1.ExecOutput_ExecFailed{ExecFailed: &pluginv1.ExecFailed{ErrorMessage: msg}}})
	}
	if u := req.GetUser(); u != "" {
		return fail(fmt.Sprintf("running steps as user %q is not supported by the kubernetes backend; set the user in the class podspec (securityContext.runAsUser) instead", u))
	}
	if len(req.GetCommand()) == 0 {
		return status.Error(codes.InvalidArgument, "empty command")
	}

	var sendMu sync.Mutex
	send := func(which pluginv1.DataChunk_Stream) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			sendMu.Lock()
			defer sendMu.Unlock()
			for off := 0; off < len(p); off += tarx.DefaultChunkSize {
				chunk := p[off:min(off+tarx.DefaultChunkSize, len(p))]
				if err := stream.Send(&pluginv1.ExecOutput{Output: &pluginv1.ExecOutput_Data{Data: &pluginv1.DataChunk{Stream: which, Data: chunk}}}); err != nil {
					return off, err
				}
			}
			return len(p), nil
		})
	}

	pidfile := fmt.Sprintf("%s/exec-%d.pid", pidDir, s.seq.Add(1))
	argv := execwrap.Wrap(req.GetCommand(), req.GetEnv(), req.GetWorkdir(), pidfile)
	code, err := s.backend.Exec(ctx, e.pod, argv, nil, send(pluginv1.DataChunk_STDOUT), send(pluginv1.DataChunk_STDERR))
	if ctx.Err() != nil {
		// Cancelled (job cancelled, step timeout): closing the exec stream
		// does not reliably kill the remote process, so kill it explicitly.
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(killGrace+10)*time.Second)
		defer cancel()
		_, _ = s.backend.Exec(kctx, e.pod, execwrap.Kill(pidfile, killGrace), nil, io.Discard, io.Discard)
		return status.FromContextError(ctx.Err()).Err()
	}
	if err != nil {
		return fail(err.Error())
	}
	return stream.Send(&pluginv1.ExecOutput{Output: &pluginv1.ExecOutput_ExecComplete{ExecComplete: &pluginv1.ExecComplete{ExitCode: int32(code)}}}) //nolint:gosec // G115: exit codes fit in int32
}

func (s *Server) CopyIn(stream pluginv1.BackendPlugin_CopyInServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "copy-in: no first chunk: %v", err)
	}
	if first.EnvironmentId == nil || first.GetEnvironmentId() == "" || first.DestPath == nil || first.GetDestPath() == "" {
		return status.Error(codes.InvalidArgument, "copy-in: the first chunk must set environment_id and dest_path")
	}
	e, unlock, err := s.acquire(ctx, first.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer unlock()
	if e.pod == "" {
		return status.Errorf(codes.FailedPrecondition, "environment %s is not started", e.id)
	}
	dest := first.GetDestPath()

	pr, pw := io.Pipe()
	var protoErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := pw.Write(first.GetData()); err != nil {
			_ = drain(stream)
			return
		}
		for {
			c, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				_ = pw.Close()
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if c.EnvironmentId != nil || c.DestPath != nil {
				protoErr = status.Error(codes.InvalidArgument, "copy-in: environment_id and dest_path are only allowed on the first chunk")
				pw.CloseWithError(protoErr)
				_ = drain(stream)
				return
			}
			if _, err := pw.Write(c.GetData()); err != nil {
				_ = drain(stream)
				return
			}
		}
	}()

	var stderr limitedBuffer
	cmd := []string{"/bin/sh", "-c", `mkdir -p "$1" && exec tar -x -f - -C "$1"`, "sh", dest}
	code, err := s.backend.Exec(ctx, e.pod, cmd, pr, io.Discard, &stderr)
	_ = pr.Close() // unblock the reader goroutine if tar stopped early
	<-done
	switch {
	case protoErr != nil:
		return protoErr
	case err != nil:
		return status.Errorf(codes.Unavailable, "copy into %s: %v", dest, err)
	case code == 127:
		return status.Errorf(codes.FailedPrecondition, "copy into %s: main container needs tar (and /bin/sh) for file copies: %s", dest, stderr.String())
	case code != 0:
		return status.Errorf(codes.Internal, "copy into %s: tar exited %d: %s", dest, code, stderr.String())
	}
	return stream.SendAndClose(&pluginv1.CopyInResponse{})
}

func drain(stream pluginv1.BackendPlugin_CopyInServer) error {
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
	}
}

func (s *Server) CopyOut(req *pluginv1.CopyOutRequest, stream pluginv1.BackendPlugin_CopyOutServer) error {
	ctx := stream.Context()
	e, unlock, err := s.acquire(ctx, req.GetEnvironmentId())
	if err != nil {
		return err
	}
	defer unlock()
	if e.pod == "" {
		return status.Errorf(codes.FailedPrecondition, "environment %s is not started", e.id)
	}
	src := path.Clean(req.GetSrcPath())
	if !path.IsAbs(src) {
		return status.Errorf(codes.InvalidArgument, "copy-out: src_path %q must be absolute", req.GetSrcPath())
	}
	dir, base := path.Dir(src), path.Base(src)
	if src == "/" {
		base = "."
	}
	w := tarx.NewChunkWriter(tarx.DefaultChunkSize, func(b []byte) error {
		return stream.Send(&pluginv1.CopyOutChunk{Data: b})
	})
	var stderr limitedBuffer
	cmd := []string{"/bin/sh", "-c", `cd "$1" && exec tar -c -f - "$2"`, "sh", dir, base}
	code, err := s.backend.Exec(ctx, e.pod, cmd, nil, w, &stderr)
	switch {
	case err != nil:
		return status.Errorf(codes.Unavailable, "copy from %s: %v", src, err)
	case code == 127:
		return status.Errorf(codes.FailedPrecondition, "copy from %s: main container needs tar (and /bin/sh) for file copies: %s", src, stderr.String())
	case code != 0 && (strings.Contains(stderr.String(), "No such file") || strings.Contains(stderr.String(), "can't cd")):
		return status.Errorf(codes.NotFound, "copy from %s: %s", src, stderr.String())
	case code != 0:
		return status.Errorf(codes.Internal, "copy from %s: tar exited %d: %s", src, code, stderr.String())
	}
	return w.Flush()
}

func (s *Server) Remove(ctx context.Context, req *pluginv1.RemoveRequest) (*pluginv1.RemoveResponse, error) {
	id := req.GetEnvironmentId()
	s.mu.Lock()
	e := s.envs[id]
	s.mu.Unlock()
	if e != nil {
		// Wait (bounded by ctx) for a cancelled RPC on this environment to
		// unwind, then forget it.
		select {
		case e.lock <- struct{}{}:
			defer func() { <-e.lock }()
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		s.mu.Lock()
		delete(s.envs, id)
		s.mu.Unlock()
	} else if len(validation.IsDNS1123Subdomain(id)) > 0 {
		return &pluginv1.RemoveResponse{}, nil // never a Job of ours
	}
	// Unknown ids still get a delete: after a plugin restart the runner
	// removes environments this process never saw. Only our own Jobs, though:
	// another instance may share the namespace.
	var err error
	if e != nil {
		err = s.backend.Delete(ctx, id)
	} else {
		err = s.backend.DeleteOwned(ctx, id, s.defaults.Instance)
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return &pluginv1.RemoveResponse{}, nil
}

// Shutdown deletes every live environment (plugin SIGTERM). It waits for an
// RPC in flight on an environment to finish first, until ctx expires; after
// that environments are deleted regardless.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	envs := make([]*environment, 0, len(s.envs))
	for _, e := range s.envs {
		envs = append(envs, e)
	}
	s.envs = map[string]*environment{}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range envs {
		wg.Go(func() {
			select {
			case e.lock <- struct{}{}:
				defer func() { <-e.lock }()
			case <-ctx.Done():
			}
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = s.backend.Delete(dctx, e.id)
		})
	}
	wg.Wait()
}

func (s *Server) acquire(ctx context.Context, id string) (*environment, func(), error) {
	s.mu.Lock()
	e := s.envs[id]
	s.mu.Unlock()
	if e == nil {
		return nil, nil, status.Errorf(codes.NotFound, "unknown environment %q", id)
	}
	select {
	case e.lock <- struct{}{}:
		return e, func() { <-e.lock }, nil
	case <-ctx.Done():
		return nil, nil, status.FromContextError(ctx.Err()).Err()
	}
}

func statusFromCtx(ctx context.Context, err error, fallback codes.Code) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(fallback, err.Error())
}

// newEnvID returns a DNS-safe Job name: a readable prefix from the runner's
// container name plus a random suffix. Kept short so pod names (Job name +
// "-xxxxx") stay within 63 characters.
func newEnvID(name string) string {
	const maxPrefix = 40
	b := []byte(strings.ToLower(name))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	prefix := strings.Trim(string(b), "-")
	if len(prefix) > maxPrefix {
		prefix = strings.Trim(prefix[:maxPrefix], "-")
	}
	if prefix == "" || prefix[0] < 'a' || prefix[0] > 'z' {
		prefix = "job-" + prefix
		prefix = strings.TrimRight(prefix, "-")
	}
	var r [4]byte
	_, _ = rand.Read(r[:])
	return prefix + "-" + hex.EncodeToString(r[:])
}

// runnerArch maps a GOARCH-style name to RUNNER_ARCH.
func runnerArch(nodeArch string) string {
	if nodeArch == "" {
		nodeArch = runtime.GOARCH
	}
	switch nodeArch {
	case "amd64":
		return "X64"
	case "arm64":
		return "ARM64"
	case "arm":
		return "ARM"
	case "386":
		return "X86"
	}
	return strings.ToUpper(nodeArch)
}

func ptrTo[T any](v T) *T { return &v }

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// limitedBuffer keeps the first stderrKeep bytes and discards the rest.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := stderrKeep - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return strings.TrimSpace(b.Buffer.String()) }
