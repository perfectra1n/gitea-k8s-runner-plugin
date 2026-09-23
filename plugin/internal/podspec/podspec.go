// Package podspec turns a per-class podspec file plus one workflow job's
// requirements (job image, services, timeout) into the batch/v1 Job that hosts
// that workflow job.
package podspec

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	// MainContainer is where every step runs.
	MainContainer = "main"
	// SharedVolume holds the workspace, act dir, tool cache and temp dir.
	SharedVolume = "shared"
	// SharedMount is where SharedVolume is mounted in main.
	SharedMount = "/__w"
	// ServicePrefix prefixes the sidecar name of each workflow service.
	ServicePrefix = "svc-"

	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "gitea-k8s-runner-plugin"
	LabelInstance  = "gitea-k8s-runner-plugin/instance"
	LabelEnvID     = "gitea-k8s-runner-plugin/environment-id"

	// SleepScript keeps main alive until the Job is deleted; steps arrive over
	// pods/exec. The trap makes SIGTERM exit promptly and cleanly.
	SleepScript = "trap 'exit 0' TERM; while :; do sleep 3600 & wait; done"

	// ttlAfterFinished garbage-collects Jobs whose Remove never arrived.
	ttlAfterFinished = 300
)

var defaultSharedSize = resource.MustParse("10Gi")

// Service is one `services:` entry of the workflow job.
type Service struct {
	Name  string
	Image string
	Env   map[string]string
	Ports []string
}

// BuildInput is everything BuildJob needs for one environment.
type BuildInput struct {
	Spec      *corev1.PodSpec
	EnvID     string
	Instance  string
	Namespace string
	// Image is the job's `container:` image; empty keeps the podspec's.
	Image    string
	Services []Service
	// Timeout becomes activeDeadlineSeconds; zero leaves it unset.
	Timeout          time.Duration
	Labels           map[string]string
	PullPolicy       corev1.PullPolicy
	ServiceResources *corev1.ResourceRequirements
}

// Load reads a podspec file (a corev1.PodSpec in YAML). Unknown fields are
// rejected so typos surface at job start instead of silently doing nothing.
func Load(path string) (*corev1.PodSpec, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator config (flag or runs-on label arg), by design
	if err != nil {
		return nil, fmt.Errorf("read podspec: %w", err)
	}
	var s corev1.PodSpec
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		return nil, fmt.Errorf("parse podspec %s: %w", path, err)
	}
	return &s, nil
}

// BuildJob renders the Job for one environment. The input podspec is not
// modified.
func BuildJob(in BuildInput) (*batchv1.Job, error) {
	if in.Spec == nil {
		return nil, fmt.Errorf("no podspec")
	}
	ps := in.Spec.DeepCopy()

	if err := setupMain(ps, in); err != nil {
		return nil, err
	}
	if err := addServices(ps, in); err != nil {
		return nil, err
	}
	addSharedVolume(ps)
	ps.RestartPolicy = corev1.RestartPolicyNever

	labels := jobLabels(in)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.EnvID,
			Namespace: in.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](ttlAfterFinished),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       *ps,
			},
		},
	}
	if in.Timeout > 0 {
		job.Spec.ActiveDeadlineSeconds = ptr.To(int64(in.Timeout / time.Second))
	}
	return job, nil
}

func setupMain(ps *corev1.PodSpec, in BuildInput) error {
	idx := -1
	for i := range ps.Containers {
		if ps.Containers[i].Name == MainContainer {
			idx = i
			break
		}
	}
	if idx < 0 {
		if in.Image == "" {
			return fmt.Errorf("podspec has no %q container and the job sets no container image", MainContainer)
		}
		ps.Containers = append([]corev1.Container{{Name: MainContainer}}, ps.Containers...)
		idx = 0
	}
	m := &ps.Containers[idx]
	if in.Image != "" {
		m.Image = in.Image
	}
	if in.PullPolicy != "" {
		m.ImagePullPolicy = in.PullPolicy
	}
	m.Command = []string{"sh", "-c", SleepScript}
	m.Args = nil
	m.WorkingDir = SharedMount
	for _, vm := range m.VolumeMounts {
		if vm.Name == SharedVolume {
			return nil
		}
	}
	m.VolumeMounts = append(m.VolumeMounts, corev1.VolumeMount{Name: SharedVolume, MountPath: SharedMount})
	return nil
}

func addServices(ps *corev1.PodSpec, in BuildInput) error {
	if len(in.Services) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(in.Services))
	for _, s := range in.Services {
		ports, err := parsePorts(s.Ports)
		if err != nil {
			return fmt.Errorf("service %s: %w", s.Name, err)
		}
		c := corev1.Container{
			Name:            ServicePrefix + dnsLabel(s.Name),
			Image:           s.Image,
			ImagePullPolicy: in.PullPolicy,
			Env:             sortedEnv(s.Env),
			Ports:           ports,
			RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways),
		}
		if in.ServiceResources != nil {
			c.Resources = *in.ServiceResources.DeepCopy()
		}
		ps.InitContainers = append(ps.InitContainers, c)
		hosts = append(hosts, s.Name)
	}
	sort.Strings(hosts)
	// Services share the pod network; let `postgres:5432` resolve like it
	// does on the docker backend.
	ps.HostAliases = append(ps.HostAliases, corev1.HostAlias{IP: "127.0.0.1", Hostnames: hosts})
	return nil
}

// parsePorts accepts docker-style mappings: "5432", "8080:80",
// "127.0.0.1:8080:80", optionally suffixed "/tcp" or "/udp". Only the container
// side matters: every container in the pod shares one network namespace.
func parsePorts(specs []string) ([]corev1.ContainerPort, error) {
	var out []corev1.ContainerPort
	for _, p := range specs {
		spec, proto, _ := strings.Cut(p, "/")
		protocol := corev1.ProtocolTCP
		switch strings.ToLower(proto) {
		case "", "tcp":
		case "udp":
			protocol = corev1.ProtocolUDP
		case "sctp":
			protocol = corev1.ProtocolSCTP
		default:
			return nil, fmt.Errorf("port %q: unknown protocol", p)
		}
		parts := strings.Split(spec, ":")
		n, err := strconv.ParseInt(parts[len(parts)-1], 10, 32)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("port %q: bad container port", p)
		}
		out = append(out, corev1.ContainerPort{ContainerPort: int32(n), Protocol: protocol})
	}
	return out, nil
}

func sortedEnv(env map[string]string) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}

// dnsLabel maps a workflow service id onto a container-name-safe DNS label.
func dnsLabel(s string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	out := strings.Trim(string(b), "-")
	if max := validation.DNS1123LabelMaxLength - len(ServicePrefix); len(out) > max {
		out = strings.TrimRight(out[:max], "-")
	}
	if out == "" {
		out = "service"
	}
	return out
}

func addSharedVolume(ps *corev1.PodSpec) {
	for _, v := range ps.Volumes {
		if v.Name == SharedVolume {
			return // the class brought its own (e.g. an ephemeral PVC)
		}
	}
	size := defaultSharedSize.DeepCopy()
	ps.Volumes = append(ps.Volumes, corev1.Volume{
		Name:         SharedVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}},
	})
}

func jobLabels(in BuildInput) map[string]string {
	l := make(map[string]string, len(in.Labels)+3)
	for k, v := range in.Labels {
		l[k] = strings.ReplaceAll(v, "${ENV_ID}", in.EnvID)
	}
	// Ownership labels last: Sweep and Remove select on them.
	l[LabelManagedBy] = ManagedByValue
	l[LabelInstance] = in.Instance
	l[LabelEnvID] = in.EnvID
	return l
}
