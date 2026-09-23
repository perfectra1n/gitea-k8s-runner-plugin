// Package k8s runs environments as batch/v1 Jobs: create, wait for the pod to
// become ready, exec into `main`, delete, and sweep orphans.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
	"k8s.io/utils/ptr"

	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/podspec"
)

// Backend is what the gRPC server needs from the cluster.
type Backend interface {
	Create(ctx context.Context, job *batchv1.Job) error
	// WaitReady blocks until the environment's pod can run commands and
	// returns its name. Bound it with ctx.
	WaitReady(ctx context.Context, envID string, progress func(string)) (podName string, err error)
	// Exec runs cmd in the pod's main container. A non-zero exit is a result,
	// not an error; err means the command could not be run or its stream broke.
	Exec(ctx context.Context, pod string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)
	Delete(ctx context.Context, envID string) error
	// Sweep deletes every Job this plugin instance left behind.
	Sweep(ctx context.Context, instance string) (int, error)
}

// KubeBackend implements Backend with client-go.
type KubeBackend struct {
	cs      kubernetes.Interface
	restCfg *rest.Config
	core    rest.Interface
	ns      string

	// PollInterval is how often WaitReady re-reads pod status.
	PollInterval time.Duration
	// PullFailAfter is how long an image-pull error may persist before
	// WaitReady gives up (registries have transient hiccups).
	PullFailAfter time.Duration
	// ServiceLogLines is how much of a crashing service's log is reported.
	ServiceLogLines int64

	newExecutor func(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error)
}

var _ Backend = (*KubeBackend)(nil)

// NewBackend returns a backend acting in namespace ns.
func NewBackend(cs kubernetes.Interface, restCfg *rest.Config, ns string) (*KubeBackend, error) {
	cfg := rest.CopyConfig(restCfg)
	setConfigDefaults(cfg)
	core, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("exec client: %w", err)
	}
	return &KubeBackend{
		cs:              cs,
		restCfg:         restCfg,
		core:            core,
		ns:              ns,
		PollInterval:    time.Second,
		PullFailAfter:   60 * time.Second,
		ServiceLogLines: 50,
		newExecutor:     newFallbackExecutor,
	}, nil
}

func setConfigDefaults(cfg *rest.Config) {
	cfg.GroupVersion = &corev1.SchemeGroupVersion
	cfg.APIPath = "/api"
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
}

// newFallbackExecutor prefers the WebSocket exec protocol and falls back to
// SPDY for API servers that do not upgrade.
func newFallbackExecutor(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	ws, err := remotecommand.NewWebSocketExecutor(cfg, "GET", u.String())
	if err != nil {
		return nil, err
	}
	spdy, err := remotecommand.NewSPDYExecutor(cfg, "POST", u)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
}

func (b *KubeBackend) Create(ctx context.Context, job *batchv1.Job) error {
	_, err := b.cs.BatchV1().Jobs(b.ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create job %s/%s: %w", b.ns, job.Name, err)
	}
	return nil
}

func (b *KubeBackend) Delete(ctx context.Context, envID string) error {
	err := b.cs.BatchV1().Jobs(b.ns).Delete(ctx, envID, metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete job %s/%s: %w", b.ns, envID, err)
	}
	return nil
}

func (b *KubeBackend) Sweep(ctx context.Context, instance string) (int, error) {
	sel := labels.SelectorFromSet(labels.Set{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelInstance:  instance,
	})
	jobs, err := b.cs.BatchV1().Jobs(b.ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return 0, fmt.Errorf("list orphan jobs: %w", err)
	}
	n := 0
	var errs []error
	for _, j := range jobs.Items {
		if err := b.Delete(ctx, j.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// WaitReady polls the environment's pod until main and every native sidecar
// are Ready, reporting each distinct pending state through progress.
func (b *KubeBackend) WaitReady(ctx context.Context, envID string, progress func(string)) (string, error) {
	sel := labels.SelectorFromSet(labels.Set{podspec.LabelEnvID: envID}).String()
	pullErrSince := map[string]time.Time{}
	last := "waiting for the job's pod to be created"
	progress(last)
	var lastPod *corev1.Pod

	tick := time.NewTicker(b.PollInterval)
	defer tick.Stop()
	for {
		pods, err := b.cs.CoreV1().Pods(b.ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		switch {
		case err != nil && ctx.Err() == nil:
			progress(fmt.Sprintf("listing pods: %v", err))
		case err == nil && len(pods.Items) > 0:
			pod := newest(pods.Items)
			lastPod = pod
			ready, state, ferr := b.assess(ctx, pod, pullErrSince)
			if ferr != nil {
				return "", ferr
			}
			if ready {
				return pod.Name, nil
			}
			if state != last {
				last = state
				progress(state)
			}
		}
		select {
		case <-ctx.Done():
			return "", b.notReadyError(ctx, lastPod, last)
		case <-tick.C:
		}
	}
}

func newest(pods []corev1.Pod) *corev1.Pod {
	sort.Slice(pods, func(i, j int) bool {
		return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
	})
	return &pods[0]
}

// assess classifies a pod as ready, still coming up (with a human-readable
// state), or failed for good.
func (b *KubeBackend) assess(ctx context.Context, pod *corev1.Pod, pullErrSince map[string]time.Time) (bool, string, error) {
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodSucceeded:
		return false, "", fmt.Errorf("pod %s ended before it was ready: phase=%s reason=%s: %s",
			pod.Name, pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
	}

	sidecars := map[string]bool{}
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars[c.Name] = true
		}
	}
	statuses := append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	var notes []string
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
				first, ok := pullErrSince[cs.Name]
				if !ok {
					first = time.Now()
					pullErrSince[cs.Name] = first
				}
				if w.Reason == "InvalidImageName" || w.Reason == "ErrImageNeverPull" || time.Since(first) >= b.PullFailAfter {
					return false, "", fmt.Errorf("image pull failed for container %s: %s: %s", cs.Name, w.Reason, w.Message)
				}
			case "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return false, "", fmt.Errorf("container %s failed to start: %s: %s%s", cs.Name, w.Reason, w.Message, b.tailLogs(ctx, pod, cs.Name))
			default:
				delete(pullErrSince, cs.Name)
			}
			notes = append(notes, fmt.Sprintf("%s: %s", cs.Name, strings.TrimSpace(w.Reason+" "+w.Message)))
			continue
		}
		delete(pullErrSince, cs.Name)
		if t := cs.State.Terminated; t != nil && (cs.Name == podspec.MainContainer || sidecars[cs.Name]) {
			return false, "", fmt.Errorf("container %s exited during startup: code %d, %s %s%s",
				cs.Name, t.ExitCode, t.Reason, t.Message, b.tailLogs(ctx, pod, cs.Name))
		}
	}

	if pod.Status.Phase == corev1.PodRunning && allReady(pod, sidecars) {
		return true, "", nil
	}
	state := fmt.Sprintf("pod %s: %s", pod.Name, pod.Status.Phase)
	for _, c := range pod.Status.Conditions {
		if c.Status == corev1.ConditionFalse && c.Reason != "" {
			state += fmt.Sprintf("; %s=%s: %s", c.Type, c.Reason, c.Message)
		}
	}
	if len(notes) > 0 {
		state += "; " + strings.Join(notes, "; ")
	}
	return false, state, nil
}

func allReady(pod *corev1.Pod, sidecars map[string]bool) bool {
	ready := map[string]bool{}
	for _, cs := range pod.Status.InitContainerStatuses {
		ready[cs.Name] = cs.Ready
	}
	for _, cs := range pod.Status.ContainerStatuses {
		ready[cs.Name] = cs.Ready
	}
	for _, c := range pod.Spec.Containers {
		if !ready[c.Name] {
			return false
		}
	}
	for name := range sidecars {
		if !ready[name] {
			return false
		}
	}
	return true
}

func (b *KubeBackend) tailLogs(ctx context.Context, pod *corev1.Pod, container string) string {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	raw, err := b.cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container, TailLines: ptr.To(b.ServiceLogLines),
	}).DoRaw(lctx)
	if err != nil || len(raw) == 0 {
		return ""
	}
	return fmt.Sprintf("\nlast %d log lines of %s:\n%s", b.ServiceLogLines, container, strings.TrimRight(string(raw), "\n"))
}

func (b *KubeBackend) notReadyError(ctx context.Context, pod *corev1.Pod, last string) error {
	msg := "pod not ready: " + last
	if pod != nil {
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if ev := b.recentEvents(ectx, pod.Name, 5); ev != "" {
			msg += "\nrecent events:\n" + ev
		}
	}
	return fmt.Errorf("%s: %w", msg, ctx.Err())
}

func (b *KubeBackend) recentEvents(ctx context.Context, podName string, n int) string {
	evs, err := b.cs.CoreV1().Events(b.ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + podName,
	})
	if err != nil {
		return ""
	}
	var items []corev1.Event
	for _, e := range evs.Items {
		if e.InvolvedObject.Name == podName { // fake clients ignore field selectors
			items = append(items, e)
		}
	}
	sort.Slice(items, func(i, j int) bool { return eventTime(items[i]).Before(eventTime(items[j])) })
	if len(items) > n {
		items = items[len(items)-n:]
	}
	var sb strings.Builder
	for _, e := range items {
		fmt.Fprintf(&sb, "  %s %s: %s\n", e.Type, e.Reason, e.Message)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// Exec runs cmd in the main container of pod.
func (b *KubeBackend) Exec(ctx context.Context, pod string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	req := b.core.Post().Resource("pods").Namespace(b.ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: podspec.MainContainer,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	ex, err := b.newExecutor(b.restCfg, req.URL())
	if err != nil {
		return -1, fmt.Errorf("exec setup: %w", err)
	}
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr})
	var exitErr utilexec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr) && exitErr.Exited():
		return exitErr.ExitStatus(), nil
	case ctx.Err() != nil:
		return -1, fmt.Errorf("exec cancelled: %w", ctx.Err())
	}
	return -1, fmt.Errorf("exec in pod %s: %w%s", pod, err, b.podState(ctx, pod))
}

// podState explains why exec may have broken: the pod was deleted, evicted, ...
func (b *KubeBackend) podState(ctx context.Context, pod string) string {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	p, err := b.cs.CoreV1().Pods(b.ns).Get(sctx, pod, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return " (the pod no longer exists: deleted or evicted)"
	case err != nil:
		return ""
	}
	s := fmt.Sprintf(" (pod phase=%s", p.Status.Phase)
	if p.Status.Reason != "" {
		s += " reason=" + p.Status.Reason
	}
	if p.Status.Message != "" {
		s += ": " + p.Status.Message
	}
	if p.DeletionTimestamp != nil {
		s += ", being deleted"
	}
	return s + ")"
}
