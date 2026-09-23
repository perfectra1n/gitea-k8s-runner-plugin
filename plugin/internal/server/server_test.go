package server

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	batchv1 "k8s.io/api/batch/v1"

	pluginv1 "github.com/perfectra1n/gitea-k8s-runner-plugin/gen/plugin/v1alpha"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/options"
)

// fakeBackend records calls; exec behaviour is scripted per test.
type fakeBackend struct {
	mu        sync.Mutex
	created   []*batchv1.Job
	deleted   []string
	execs     [][]string
	readyErr  error
	readyWait chan struct{}
	exec      func(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

func (f *fakeBackend) Create(_ context.Context, j *batchv1.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, j)
	return nil
}

func (f *fakeBackend) WaitReady(ctx context.Context, envID string, progress func(string)) (string, error) {
	progress("pod pending: Unschedulable")
	if f.readyWait != nil {
		select {
		case <-f.readyWait:
		case <-ctx.Done():
			return "", fmt.Errorf("pod not ready: Unschedulable: %w", ctx.Err())
		}
	}
	if f.readyErr != nil {
		return "", f.readyErr
	}
	return envID + "-pod", nil
}

func (f *fakeBackend) Exec(ctx context.Context, _ string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.execs = append(f.execs, cmd)
	fn := f.exec
	f.mu.Unlock()
	if fn == nil {
		return 0, nil
	}
	return fn(ctx, cmd, stdin, stdout, stderr)
}

func (f *fakeBackend) Delete(_ context.Context, envID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, envID)
	return nil
}

func (f *fakeBackend) Sweep(context.Context, string) (int, error) { return 0, nil }

func (f *fakeBackend) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeBackend) execCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.execs...)
}

const testPodspec = "containers: [{name: main, image: ghcr.io/catthehacker/ubuntu:act-24.04}]\n"

func setup(t *testing.T, fb *fakeBackend) (pluginv1.BackendPluginClient, *Server) {
	t.Helper()
	dir := t.TempDir()
	ps := filepath.Join(dir, "podspec.yaml")
	if err := os.WriteFile(ps, []byte(testPodspec), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(fb, options.Defaults{Namespace: "ci", Instance: "r0", Podspec: ps})
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pluginv1.RegisterBackendPluginServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pluginv1.NewBackendPluginClient(conn), srv
}

func create(t *testing.T, c pluginv1.BackendPluginClient, req *pluginv1.CreateRequest) *pluginv1.CreateResponse {
	t.Helper()
	if req == nil {
		req = &pluginv1.CreateRequest{Name: "GITEA-ACTIONS-TASK-42_build"}
	}
	resp, err := c.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func start(t *testing.T, c pluginv1.BackendPluginClient, id string) (*pluginv1.StartComplete, []string, error) {
	t.Helper()
	st, err := c.Start(context.Background(), &pluginv1.StartRequest{EnvironmentId: id})
	if err != nil {
		return nil, nil, err
	}
	var progress []string
	for {
		m, err := st.Recv()
		if err != nil {
			return nil, progress, err
		}
		if d := m.GetData(); d != nil {
			progress = append(progress, string(d.GetData()))
		}
		if sc := m.GetStartComplete(); sc != nil {
			if _, err := st.Recv(); !errors.Is(err, io.EOF) {
				t.Errorf("stream should end after StartComplete, got %v", err)
			}
			return sc, progress, nil
		}
	}
}

func started(t *testing.T, fb *fakeBackend) (pluginv1.BackendPluginClient, *Server, string) {
	t.Helper()
	c, srv := setup(t, fb)
	id := create(t, c, nil).GetEnvironmentId()
	if _, _, err := start(t, c, id); err != nil {
		t.Fatal(err)
	}
	return c, srv, id
}

type execResult struct {
	stdout, stderr string
	complete       *pluginv1.ExecComplete
	failed         *pluginv1.ExecFailed
}

func execRPC(t *testing.T, c pluginv1.BackendPluginClient, req *pluginv1.ExecRequest) execResult {
	t.Helper()
	st, err := c.Exec(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var r execResult
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return r
		}
		if err != nil {
			t.Fatal(err)
		}
		switch o := m.Output.(type) {
		case *pluginv1.ExecOutput_Data:
			if o.Data.GetStream() == pluginv1.DataChunk_STDERR {
				r.stderr += string(o.Data.GetData())
			} else {
				r.stdout += string(o.Data.GetData())
			}
		case *pluginv1.ExecOutput_ExecComplete:
			r.complete = o.ExecComplete
		case *pluginv1.ExecOutput_ExecFailed:
			r.failed = o.ExecFailed
		}
	}
}

func TestCapabilities(t *testing.T) {
	c, _ := setup(t, &fakeBackend{})
	r, err := c.Capabilities(context.Background(), &pluginv1.CapabilitiesRequest{})
	if err != nil || r.GetName() != "kubernetes" {
		t.Fatalf("%v %v", r, err)
	}
}

func TestCreateReportsLayoutAndBuildsJob(t *testing.T) {
	fb := &fakeBackend{}
	c, _ := setup(t, fb)
	resp := create(t, c, &pluginv1.CreateRequest{
		Name:               "GITEA-ACTIONS-TASK-42_build",
		Image:              "alpine:3",
		Services:           []*pluginv1.ServiceContainer{{Name: "postgres", Image: "postgres:17", Ports: []string{"5432"}}},
		EnvironmentTimeout: durationpb.New(90 * time.Minute),
		BackendOptions:     map[string]string{"labels": "team=ci"},
	})
	id := resp.GetEnvironmentId()
	if !strings.HasPrefix(id, "gitea-actions-task-42-build-") || len(id) > 52 {
		t.Errorf("environment id %q should be a readable DNS-safe job name", id)
	}
	if resp.GetRootPath() != "/__w" || resp.GetActPath() != "/__w/_act" || resp.GetToolCachePath() != "/__w/_tool" ||
		resp.GetTempPath() != "/__w/_temp" || resp.GetOs() != "Linux" || resp.GetArch() == "" {
		t.Errorf("layout = %v", resp)
	}
	if resp.GetPathVariableName() != "PATH" || resp.GetPathSeparator() != ":" || resp.GetDefaultPathVariable() == "" {
		t.Errorf("path conventions = %v", resp)
	}
	if len(fb.created) != 0 {
		t.Error("Create must not create the Job; Start does")
	}
	if _, _, err := start(t, c, id); err != nil {
		t.Fatal(err)
	}
	j := fb.created[0]
	ps := j.Spec.Template.Spec
	if ps.Containers[0].Image != "alpine:3" || ps.InitContainers[0].Name != "svc-postgres" {
		t.Errorf("job = %+v", ps)
	}
	if *j.Spec.ActiveDeadlineSeconds != 5400 || j.Labels["team"] != "ci" || j.Namespace != "ci" {
		t.Errorf("job meta/spec = %v %v", j.Labels, j.Spec.ActiveDeadlineSeconds)
	}
}

func TestCreateLabelArgSelectsPodspec(t *testing.T) {
	fb := &fakeBackend{}
	c, _ := setup(t, fb)
	other := filepath.Join(t.TempDir(), "big.yaml")
	if err := os.WriteFile(other, []byte("containers: [{name: main, image: big:1}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := create(t, c, &pluginv1.CreateRequest{Name: "x", LabelArg: other}).GetEnvironmentId()
	if _, _, err := start(t, c, id); err != nil {
		t.Fatal(err)
	}
	if img := fb.created[0].Spec.Template.Spec.Containers[0].Image; img != "big:1" {
		t.Errorf("image = %q, want the label_arg podspec's", img)
	}
}

func TestCreateInvalidArgument(t *testing.T) {
	c, _ := setup(t, &fakeBackend{})
	for name, req := range map[string]*pluginv1.CreateRequest{
		"unknown option":  {Name: "x", BackendOptions: map[string]string{"bogus": "1"}},
		"missing podspec": {Name: "x", LabelArg: "/nonexistent/podspec.yaml"},
		"bad port":        {Name: "x", Services: []*pluginv1.ServiceContainer{{Name: "s", Image: "i", Ports: []string{"http"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Create(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("err = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestStartStreamsProgressAndImageEnv(t *testing.T) {
	fb := &fakeBackend{exec: func(_ context.Context, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		if cmd[0] == "env" {
			_, _ = io.WriteString(stdout, "PATH=/usr/bin:/bin\x00HOSTNAME=pod\x00MULTI=a\nb\x00JAVA_HOME=/opt/java\x00")
		}
		return 0, nil
	}}
	c, _ := setup(t, fb)
	id := create(t, c, nil).GetEnvironmentId()
	sc, progress, err := start(t, c, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(progress, ""), "Unschedulable") {
		t.Errorf("progress = %q", progress)
	}
	env := sc.GetImageEnv()
	if env["PATH"] != "/usr/bin:/bin" || env["MULTI"] != "a\nb" || env["JAVA_HOME"] != "/opt/java" {
		t.Errorf("image env = %v", env)
	}
	if _, ok := env["HOSTNAME"]; ok {
		t.Error("HOSTNAME is per-pod noise and must not leak into the job env")
	}
	var mkdir bool
	for _, cmd := range fb.execCalls() {
		if j := strings.Join(cmd, " "); strings.Contains(j, "/__w/_act") && strings.Contains(j, "/__w/_temp") {
			mkdir = true
		}
	}
	if !mkdir {
		t.Errorf("start must create the layout dirs; execs = %q", fb.execCalls())
	}
}

// Review Focus 2 (server side): a readiness failure deletes the Job and the
// error text reaches the runner.
func TestStartFailureDeletesJob(t *testing.T) {
	fb := &fakeBackend{readyErr: errors.New("pod not ready: Unschedulable: 0/3 nodes: deadline exceeded")}
	c, _ := setup(t, fb)
	id := create(t, c, nil).GetEnvironmentId()
	_, _, err := start(t, c, id)
	if err == nil || !strings.Contains(status.Convert(err).Message(), "0/3 nodes") {
		t.Fatalf("err = %v", err)
	}
	if d := fb.deletedIDs(); len(d) != 1 || d[0] != id {
		t.Errorf("deleted = %v, want [%s]", d, id)
	}
}

func TestStartReadyTimeoutFromOptions(t *testing.T) {
	fb := &fakeBackend{readyWait: make(chan struct{})}
	c, _ := setup(t, fb)
	id := create(t, c, &pluginv1.CreateRequest{Name: "x", BackendOptions: map[string]string{"ready_timeout": "50ms"}}).GetEnvironmentId()
	_, _, err := start(t, c, id)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if len(fb.deletedIDs()) != 1 {
		t.Error("timed-out job must be deleted")
	}
}

// Review Focus 1 (exec side): main without /bin/sh fails with a clear message.
func TestStartMissingShell(t *testing.T) {
	fb := &fakeBackend{exec: func(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return -1, errors.New(`exec: "/bin/sh": stat /bin/sh: no such file or directory: unknown`)
	}}
	c, _ := setup(t, fb)
	id := create(t, c, nil).GetEnvironmentId()
	_, _, err := start(t, c, id)
	if err == nil || !strings.Contains(err.Error(), "main container needs /bin/sh") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownEnvironmentIsNotFound(t *testing.T) {
	c, _ := setup(t, &fakeBackend{})
	_, _, err := start(t, c, "nope")
	if status.Code(err) != codes.NotFound {
		t.Errorf("start: %v", err)
	}
	st, _ := c.Exec(context.Background(), &pluginv1.ExecRequest{EnvironmentId: "nope", Command: []string{"true"}})
	if _, err := st.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("exec: %v", err)
	}
}

// Review Focus 4 (server side): a non-zero exit is ExecComplete, a transport
// failure is ExecFailed.
func TestExecExitCodeVersusFailure(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	fb.mu.Lock()
	fb.exec = func(_ context.Context, cmd []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
		switch cmd[len(cmd)-1] {
		case "fail3":
			_, _ = io.WriteString(stdout, "out")
			_, _ = io.WriteString(stderr, "err")
			return 3, nil
		case "broken":
			return -1, errors.New("exec in pod p: connection reset (pod phase=Failed reason=Evicted)")
		}
		return 0, nil
	}
	fb.mu.Unlock()

	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"sh", "-c", "fail3"}})
	if r.complete == nil || r.complete.GetExitCode() != 3 || r.failed != nil {
		t.Fatalf("exit 3: %+v", r)
	}
	if r.stdout != "out" || r.stderr != "err" {
		t.Errorf("streams: %q %q", r.stdout, r.stderr)
	}
	r = execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"broken"}})
	if r.failed == nil || r.complete != nil || !strings.Contains(r.failed.GetErrorMessage(), "Evicted") {
		t.Fatalf("broken: %+v", r)
	}
	r = execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"ok"}})
	if r.complete == nil || r.complete.GetExitCode() != 0 {
		t.Fatalf("ok: %+v", r)
	}
}

func TestExecWrapsEnvAndWorkdir(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"node", "x.js"},
		Env: map[string]string{"A": "1"}, Workdir: "/__w/o/r"})
	calls := fb.execCalls()
	got := calls[len(calls)-1]
	j := strings.Join(got, "\x00")
	if got[0] != "/bin/sh" || !strings.Contains(j, "\x00A=1\x00") || !strings.Contains(j, "\x00/__w/o/r\x00") ||
		!strings.HasSuffix(j, "\x00node\x00x.js") {
		t.Errorf("wrapped argv = %q", got)
	}
}

func TestExecUserRule(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	r := execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"id"}, User: proto.String("1001")})
	if r.failed == nil || !strings.Contains(r.failed.GetErrorMessage(), "user") {
		t.Fatalf("non-empty user must be ExecFailed: %+v", r)
	}
	r = execRPC(t, c, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"id"}, User: proto.String("")})
	if r.complete == nil {
		t.Fatalf("empty user is the default user: %+v", r)
	}
}

func TestExecCancelKillsCommand(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	running := make(chan struct{})
	fb.mu.Lock()
	fb.exec = func(ctx context.Context, cmd []string, _ io.Reader, _, _ io.Writer) (int, error) {
		if cmd[len(cmd)-1] == "sleep-forever" {
			close(running)
			<-ctx.Done()
			return -1, ctx.Err()
		}
		return 0, nil
	}
	fb.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.Exec(ctx, &pluginv1.ExecRequest{EnvironmentId: id, Command: []string{"sleep-forever"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = st.Recv() }()
	<-running
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		for _, cmd := range fb.execCalls() {
			if strings.Contains(strings.Join(cmd, " "), "kill -TERM") {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("no kill exec after cancel; execs = %q", fb.execCalls())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for n, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCopyInPipesTarToExtract(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	var got []byte
	var gotCmd []string
	fb.mu.Lock()
	fb.exec = func(_ context.Context, cmd []string, stdin io.Reader, _, _ io.Writer) (int, error) {
		gotCmd = cmd
		got, _ = io.ReadAll(stdin)
		return 0, nil
	}
	fb.mu.Unlock()
	archive := tarOf(t, map[string]string{"workflow/event.json": "{}"})
	st, err := c.CopyIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	half := len(archive) / 2
	if err := st.Send(&pluginv1.CopyInChunk{EnvironmentId: proto.String(id), DestPath: proto.String("/__w/_act/"), Data: archive[:half]}); err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pluginv1.CopyInChunk{Data: archive[half:]}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Errorf("extracted %d bytes, want %d", len(got), len(archive))
	}
	j := strings.Join(gotCmd, " ")
	if !strings.Contains(j, "tar") || !strings.Contains(j, "/__w/_act/") || !strings.Contains(j, "mkdir -p") {
		t.Errorf("copy-in command = %q", gotCmd)
	}
}

func TestCopyInFirstChunkRules(t *testing.T) {
	fb := &fakeBackend{exec: func(_ context.Context, _ []string, stdin io.Reader, _, _ io.Writer) (int, error) {
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		return 0, nil
	}}
	c, _, id := started(t, fb)
	send := func(chunks ...*pluginv1.CopyInChunk) error {
		st, err := c.CopyIn(context.Background())
		if err != nil {
			return err
		}
		for _, ch := range chunks {
			if err := st.Send(ch); err != nil {
				break
			}
		}
		_, err = st.CloseAndRecv()
		return err
	}
	if err := send(&pluginv1.CopyInChunk{DestPath: proto.String("/x"), Data: []byte("a")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing env id: %v", err)
	}
	if err := send(&pluginv1.CopyInChunk{EnvironmentId: proto.String(id), Data: []byte("a")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing dest: %v", err)
	}
	err := send(
		&pluginv1.CopyInChunk{EnvironmentId: proto.String(id), DestPath: proto.String("/x"), Data: []byte("a")},
		&pluginv1.CopyInChunk{EnvironmentId: proto.String(id), Data: []byte("b")},
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("env id on a later chunk: %v", err)
	}
}

// Review Focus 1 (copy side): no tar in main.
func TestCopyInMissingTar(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	fb.mu.Lock()
	fb.exec = func(_ context.Context, _ []string, stdin io.Reader, _, stderr io.Writer) (int, error) {
		_, _ = io.Copy(io.Discard, stdin)
		_, _ = io.WriteString(stderr, "sh: 1: tar: not found\n")
		return 127, nil
	}
	fb.mu.Unlock()
	st, _ := c.CopyIn(context.Background())
	_ = st.Send(&pluginv1.CopyInChunk{EnvironmentId: proto.String(id), DestPath: proto.String("/x"), Data: []byte("a")})
	_, err := st.CloseAndRecv()
	if err == nil || !strings.Contains(err.Error(), "main container needs tar") || !strings.Contains(err.Error(), "tar: not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestCopyOutChunks(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	payload := bytes.Repeat([]byte("x"), 150*1024)
	var gotCmd []string
	fb.mu.Lock()
	fb.exec = func(_ context.Context, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		gotCmd = cmd
		_, _ = stdout.Write(payload)
		return 0, nil
	}
	fb.mu.Unlock()
	st, err := c.CopyOut(context.Background(), &pluginv1.CopyOutRequest{EnvironmentId: id, SrcPath: "/__w/_temp/env"})
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	var got []byte
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(m.GetData()))
		got = append(got, m.GetData()...)
	}
	if fmt.Sprint(sizes) != "[65536 65536 22528]" || !bytes.Equal(got, payload) {
		t.Errorf("chunk sizes = %v", sizes)
	}
	if n := len(gotCmd); n < 2 || !strings.Contains(strings.Join(gotCmd, " "), "tar -c") || gotCmd[n-2] != "/__w/_temp" || gotCmd[n-1] != "env" {
		t.Errorf("copy-out command = %q", gotCmd)
	}
}

func TestCopyOutMissingPath(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	fb.mu.Lock()
	fb.exec = func(_ context.Context, _ []string, _ io.Reader, _, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stderr, "tar: env: Cannot stat: No such file or directory")
		return 2, nil
	}
	fb.mu.Unlock()
	st, _ := c.CopyOut(context.Background(), &pluginv1.CopyOutRequest{EnvironmentId: id, SrcPath: "/nope"})
	_, err := st.Recv()
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestRemoveIdempotent(t *testing.T) {
	fb := &fakeBackend{}
	c, _, id := started(t, fb)
	for i := range 2 {
		if _, err := c.Remove(context.Background(), &pluginv1.RemoveRequest{EnvironmentId: id}); err != nil {
			t.Fatalf("remove #%d: %v", i+1, err)
		}
	}
	// A Remove for an id this process never saw (plugin restarted) still
	// deletes by name and succeeds.
	if _, err := c.Remove(context.Background(), &pluginv1.RemoveRequest{EnvironmentId: "gk-from-before-restart"}); err != nil {
		t.Fatal(err)
	}
	if d := fb.deletedIDs(); len(d) < 2 || d[len(d)-1] != "gk-from-before-restart" {
		t.Errorf("deleted = %v", d)
	}
}

func TestShutdownRemovesLiveEnvironments(t *testing.T) {
	fb := &fakeBackend{}
	c, srv, id := started(t, fb)
	id2 := create(t, c, nil).GetEnvironmentId()
	srv.Shutdown(context.Background())
	d := strings.Join(fb.deletedIDs(), ",")
	if !strings.Contains(d, id) || !strings.Contains(d, id2) {
		t.Errorf("deleted = %s, want both %s and %s", d, id, id2)
	}
}
