package k8s

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"

	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/podspec"
)

const (
	ns    = "ci"
	envID = "gk-test"
)

func newTestBackend(t *testing.T, objs ...runtime.Object) (*KubeBackend, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	b, err := NewBackend(cs, &rest.Config{Host: "https://k8s.invalid"}, ns)
	if err != nil {
		t.Fatal(err)
	}
	b.PollInterval = 5 * time.Millisecond
	b.PullFailAfter = 60 * time.Millisecond
	return b, cs
}

func podLabels(env, instance string) map[string]string {
	return map[string]string{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelInstance:  instance,
		podspec.LabelEnvID:     env,
	}
}

func basePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: envID + "-abcde", Namespace: ns, Labels: podLabels(envID, "r0")},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "svc-db", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)}},
			Containers:     []corev1.Container{{Name: podspec.MainContainer}},
		},
	}
}

func withStatus(p *corev1.Pod, st corev1.PodStatus) *corev1.Pod {
	p = p.DeepCopy()
	p.Status = st
	return p
}

func pending(reason, msg string) corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: reason, Message: msg,
	}}}
}

func waiting(name, reason, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}}
}

func running(name string, ready bool) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, Ready: ready, Started: ptr.To(true), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

func readyStatus() corev1.PodStatus {
	return corev1.PodStatus{
		Phase:                 corev1.PodRunning,
		InitContainerStatuses: []corev1.ContainerStatus{running("svc-db", true)},
		ContainerStatuses:     []corev1.ContainerStatus{running(podspec.MainContainer, true)},
	}
}

// script serves successive pod states to pod list calls; the last one sticks.
func script(cs *fake.Clientset, pods ...*corev1.Pod) {
	var mu sync.Mutex
	i := 0
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		p := pods[min(i, len(pods)-1)]
		i++
		l := &corev1.PodList{}
		if p != nil {
			l.Items = []corev1.Pod{*p}
		}
		return true, l, nil
	})
}

func collect() (func(string), func() []string) {
	var mu sync.Mutex
	var lines []string
	return func(s string) {
			mu.Lock()
			lines = append(lines, s)
			mu.Unlock()
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), lines...)
		}
}

func TestWaitReadySucceedsWhenMainAndSidecarsReady(t *testing.T) {
	b, cs := newTestBackend(t)
	p := basePod()
	script(cs,
		nil,
		withStatus(p, pending("Unschedulable", "0/3 nodes are available")),
		withStatus(p, corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{waiting("svc-db", "ContainerCreating", "")}}),
		withStatus(p, corev1.PodStatus{Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{running("svc-db", false)},
			ContainerStatuses:     []corev1.ContainerStatus{running(podspec.MainContainer, true)}}),
		withStatus(p, readyStatus()),
	)
	progress, lines := collect()
	name, err := b.WaitReady(context.Background(), envID, progress)
	if err != nil {
		t.Fatal(err)
	}
	if name != p.Name {
		t.Errorf("pod = %q", name)
	}
	all := strings.Join(lines(), "\n")
	for _, want := range []string{"Unschedulable", "0/3 nodes are available", "svc-db", "ContainerCreating"} {
		if !strings.Contains(all, want) {
			t.Errorf("progress %q lacks %q", all, want)
		}
	}
	// Identical consecutive states are reported once.
	seen := map[string]int{}
	for _, l := range lines() {
		seen[l]++
	}
	for l, n := range seen {
		if n > 1 {
			t.Errorf("progress line repeated %d times: %q", n, l)
		}
	}
}

// Review Focus 2: stuck Pending under a capacity scheduler past ready_timeout.
func TestWaitReadyTimeoutReportsConditionAndEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: envID + "-abcde", Namespace: ns},
		Reason:         "FailedScheduling", Message: "queue ci-low is full", Type: corev1.EventTypeWarning,
		LastTimestamp: metav1.Now(),
	}
	b, cs := newTestBackend(t, ev)
	script(cs, withStatus(basePod(), pending("Unschedulable", "waiting for capacity")))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := b.WaitReady(ctx, envID, func(string) {})
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error should wrap DeadlineExceeded: %v", err)
	}
	for _, want := range []string{"not ready", "Unschedulable", "waiting for capacity", "FailedScheduling", "queue ci-low is full"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestWaitReadyFailsFastOnPersistentImagePull(t *testing.T) {
	b, cs := newTestBackend(t)
	script(cs, withStatus(basePod(), corev1.PodStatus{Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{waiting(podspec.MainContainer, "ErrImagePull", "manifest unknown")}}))
	start := time.Now()
	_, err := b.WaitReady(context.Background(), envID, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") || !strings.Contains(err.Error(), "main") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) < b.PullFailAfter {
		t.Error("gave up before PullFailAfter; a transient pull error must be tolerated")
	}
}

func TestWaitReadyToleratesTransientImagePull(t *testing.T) {
	b, cs := newTestBackend(t)
	p := basePod()
	script(cs,
		withStatus(p, corev1.PodStatus{Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{waiting(podspec.MainContainer, "ImagePullBackOff", "rate limited")}}),
		withStatus(p, readyStatus()),
	)
	if _, err := b.WaitReady(context.Background(), envID, func(string) {}); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyServiceCrashLoopIncludesNameAndLogs(t *testing.T) {
	b, cs := newTestBackend(t)
	st := readyStatus()
	crash := waiting("svc-db", "CrashLoopBackOff", "back-off 10s")
	crash.RestartCount = b.SidecarRestartLimit
	st.InitContainerStatuses = []corev1.ContainerStatus{crash}
	script(cs, withStatus(basePod(), st))
	_, err := b.WaitReady(context.Background(), envID, func(string) {})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"svc-db", "CrashLoopBackOff", "fake logs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func terminated(name string, code int32, reason string, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, RestartCount: restarts, State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason},
	}}
}

func restarted(cs corev1.ContainerStatus, n int32) corev1.ContainerStatus {
	cs.RestartCount = n
	return cs
}

// Kubelet restarts native sidecars by design, so a sidecar exit is only fatal
// once it has used up SidecarRestartLimit restarts; main stays strict.
func TestAssessSidecarRestarts(t *testing.T) {
	const limit = 3
	tests := []struct {
		name      string
		init      corev1.ContainerStatus
		main      corev1.ContainerStatus
		wantErr   []string
		wantState string
	}{
		{
			name:      "sidecar terminated once",
			init:      terminated("svc-db", 0, "Completed", 0),
			main:      running(podspec.MainContainer, true),
			wantState: "svc-db: restarting (1/3)",
		},
		{
			name:      "sidecar crash loop below limit",
			init:      restarted(waiting("svc-db", "CrashLoopBackOff", "back-off 20s"), limit-1),
			main:      running(podspec.MainContainer, true),
			wantState: "svc-db: restarting (3/3)",
		},
		{
			name:    "sidecar crash loop at limit",
			init:    restarted(waiting("svc-db", "CrashLoopBackOff", "back-off 1m20s"), limit),
			main:    running(podspec.MainContainer, true),
			wantErr: []string{"container svc-db failed to start", "CrashLoopBackOff", "back-off 1m20s", "fake logs"},
		},
		{
			name:    "sidecar terminated past limit",
			init:    terminated("svc-db", 0, "Completed", limit+1),
			main:    running(podspec.MainContainer, true),
			wantErr: []string{"container svc-db exited during startup", "code 0", "Completed", "fake logs"},
		},
		{
			name:    "main terminated is fatal immediately",
			init:    running("svc-db", true),
			main:    terminated(podspec.MainContainer, 1, "Error", 0),
			wantErr: []string{"container main exited during startup", "code 1", "Error"},
		},
		{
			name:    "main crash loop is fatal immediately",
			init:    running("svc-db", true),
			main:    waiting(podspec.MainContainer, "CrashLoopBackOff", "back-off 10s"),
			wantErr: []string{"container main failed to start", "CrashLoopBackOff"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newTestBackend(t)
			b.SidecarRestartLimit = limit
			pod := withStatus(basePod(), corev1.PodStatus{
				Phase:                 corev1.PodRunning,
				InitContainerStatuses: []corev1.ContainerStatus{tt.init},
				ContainerStatuses:     []corev1.ContainerStatus{tt.main},
			})
			ready, state, err := b.assess(context.Background(), pod, map[string]time.Time{})
			if ready {
				t.Fatal("assess reported ready")
			}
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("want progress, got fatal error: %v", err)
				}
				if !strings.Contains(state, tt.wantState) {
					t.Errorf("state %q lacks %q", state, tt.wantState)
				}
				return
			}
			if err == nil {
				t.Fatalf("want fatal error, got state %q", state)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

// A sidecar that recovers from a restart within the limit lets the job run.
func TestWaitReadyRecoversFromSidecarRestart(t *testing.T) {
	b, cs := newTestBackend(t)
	p := basePod()
	script(cs,
		withStatus(p, corev1.PodStatus{Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{terminated("svc-db", 0, "Completed", 0)},
			ContainerStatuses:     []corev1.ContainerStatus{running(podspec.MainContainer, true)}}),
		withStatus(p, corev1.PodStatus{Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{restarted(waiting("svc-db", "CrashLoopBackOff", "back-off 10s"), 1)},
			ContainerStatuses:     []corev1.ContainerStatus{running(podspec.MainContainer, true)}}),
		withStatus(p, readyStatus()),
	)
	progress, lines := collect()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.WaitReady(ctx, envID, progress); err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(lines(), "\n"); !strings.Contains(all, "svc-db: restarting (2/3)") {
		t.Errorf("progress %q lacks the restart", all)
	}
}

func TestWaitReadyPodFailed(t *testing.T) {
	b, cs := newTestBackend(t)
	script(cs, withStatus(basePod(), corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "low on ephemeral-storage"}))
	_, err := b.WaitReady(context.Background(), envID, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "Evicted") || !strings.Contains(err.Error(), "ephemeral-storage") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateAndDeleteIdempotent(t *testing.T) {
	b, cs := newTestBackend(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: envID, Namespace: ns, Labels: podLabels(envID, "r0")}}
	if err := b.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.BatchV1().Jobs(ns).Get(context.Background(), envID, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := b.Delete(context.Background(), envID); err != nil {
			t.Fatalf("delete #%d: %v", i+1, err)
		}
	}
	if _, err := cs.BatchV1().Jobs(ns).Get(context.Background(), envID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("job still present: %v", err)
	}
	var fg bool
	for _, a := range cs.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && d.GetResource().Resource == "jobs" {
			if o := d.GetDeleteOptions(); o.PropagationPolicy != nil && *o.PropagationPolicy == metav1.DeletePropagationForeground {
				fg = true
			}
		}
	}
	if !fg {
		t.Error("delete must use foreground propagation")
	}
}

// Review Focus 3: sweep deletes only this instance's Jobs.
func TestSweepOnlyOwnInstance(t *testing.T) {
	job := func(name, instance string, managed bool) *batchv1.Job {
		l := podLabels(name, instance)
		if !managed {
			delete(l, podspec.LabelManagedBy)
		}
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: l}}
	}
	b, cs := newTestBackend(t,
		job("mine-1", "r0", true), job("mine-2", "r0", true),
		job("theirs", "r1", true), job("unmanaged", "r0", false),
	)
	n, err := b.Sweep(context.Background(), "r0")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("swept %d, want 2", n)
	}
	left, _ := cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{})
	var names []string
	for _, j := range left.Items {
		names = append(names, j.Name)
	}
	if strings.Join(names, ",") != "theirs,unmanaged" {
		t.Errorf("remaining = %v", names)
	}
}

type fakeExecutor struct {
	run func(opts remotecommand.StreamOptions) error
	url *url.URL
}

func (f *fakeExecutor) Stream(opts remotecommand.StreamOptions) error { return f.run(opts) }
func (f *fakeExecutor) StreamWithContext(_ context.Context, opts remotecommand.StreamOptions) error {
	return f.run(opts)
}

func TestExecExitCodesAndOutput(t *testing.T) {
	b, _ := newTestBackend(t)
	var got *fakeExecutor
	b.newExecutor = func(_ *rest.Config, u *url.URL) (remotecommand.Executor, error) {
		got = &fakeExecutor{url: u, run: func(o remotecommand.StreamOptions) error {
			in, _ := io.ReadAll(o.Stdin)
			_, _ = o.Stdout.Write(append([]byte("out:"), in...))
			_, _ = o.Stderr.Write([]byte("err"))
			return utilexec.CodeExitError{Err: errors.New("command terminated with exit code 3"), Code: 3}
		}}
		return got, nil
	}
	var out, errb bytes.Buffer
	code, err := b.Exec(context.Background(), "pod-1", []string{"sh", "-c", "x"}, strings.NewReader("in"), &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || out.String() != "out:in" || errb.String() != "err" {
		t.Errorf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	q := got.url.Query()
	if got.url.Path != "/api/v1/namespaces/ci/pods/pod-1/exec" || q.Get("container") != "main" ||
		strings.Join(q["command"], " ") != "sh -c x" || q.Get("stdin") != "true" {
		t.Errorf("exec url = %s", got.url)
	}
}

func TestExecSuccessIsZero(t *testing.T) {
	b, _ := newTestBackend(t)
	b.newExecutor = func(*rest.Config, *url.URL) (remotecommand.Executor, error) {
		return &fakeExecutor{run: func(remotecommand.StreamOptions) error { return nil }}, nil
	}
	code, err := b.Exec(context.Background(), "p", []string{"true"}, nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

// Spec error handling: eviction/deletion mid-job names the pod phase and reason.
func TestExecTransportErrorNamesPodState(t *testing.T) {
	pod := withStatus(basePod(), corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "node drained"})
	b, _ := newTestBackend(t, pod)
	b.newExecutor = func(*rest.Config, *url.URL) (remotecommand.Executor, error) {
		return &fakeExecutor{run: func(remotecommand.StreamOptions) error { return errors.New("connection reset") }}, nil
	}
	_, err := b.Exec(context.Background(), pod.Name, []string{"true"}, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"connection reset", "Failed", "Evicted", "node drained"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	_, err = b.Exec(context.Background(), "gone", []string{"true"}, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("err = %v", err)
	}
}

// Deleting an environment this process never tracked must not touch Jobs of
// another plugin instance sharing the namespace.
func TestDeleteOwned(t *testing.T) {
	mk := func(name, instance string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: podLabels(name, instance)}}
	}
	unmanaged := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: ns}}
	b, cs := newTestBackend(t, mk("mine", "r0"), mk("theirs", "r1"), unmanaged)
	for _, id := range []string{"mine", "theirs", "unmanaged", "missing"} {
		if err := b.DeleteOwned(context.Background(), id, "r0"); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	left, _ := cs.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{})
	var names []string
	for _, j := range left.Items {
		names = append(names, j.Name)
	}
	if strings.Join(names, ",") != "theirs,unmanaged" {
		t.Errorf("remaining = %v, want theirs,unmanaged", names)
	}
}
