package podspec

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

func mustSpec(t *testing.T, y string) *corev1.PodSpec {
	t.Helper()
	var s corev1.PodSpec
	if err := yaml.UnmarshalStrict([]byte(y), &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func build(t *testing.T, in BuildInput) *batchv1.Job {
	t.Helper()
	if in.EnvID == "" {
		in.EnvID = "gk-abc123"
	}
	if in.Namespace == "" {
		in.Namespace = "ci"
	}
	if in.Instance == "" {
		in.Instance = "runner-0"
	}
	j, err := BuildJob(in)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func container(t *testing.T, cs []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("container %q not found in %v", name, names(cs))
	return corev1.Container{}
}

func names(cs []corev1.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

const plain = `
containers:
- name: main
  image: ghcr.io/catthehacker/ubuntu:act-24.04
  env: [{name: FOO, value: bar}]
`

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "podspec.yaml")
	if err := os.WriteFile(p, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Containers[0].Image != "ghcr.io/catthehacker/ubuntu:act-24.04" {
		t.Errorf("image = %q", s.Containers[0].Image)
	}
	if err := os.WriteFile(p, []byte("containers: []\nbogusField: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("unknown field should be rejected")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file should be an error")
	}
}

func TestMainContainer(t *testing.T) {
	cases := []struct {
		name, spec, image, wantImage string
		wantFirst                    bool
	}{
		{"absent is prepended with job image", "containers: [{name: dind, image: docker:dind}]", "alpine:3", "alpine:3", true},
		{"present keeps podspec image", plain, "", "ghcr.io/catthehacker/ubuntu:act-24.04", true},
		{"present is overridden by container:", plain, "alpine:3", "alpine:3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := build(t, BuildInput{Spec: mustSpec(t, tc.spec), Image: tc.image})
			cs := j.Spec.Template.Spec.Containers
			m := container(t, cs, MainContainer)
			if m.Image != tc.wantImage {
				t.Errorf("image = %q, want %q", m.Image, tc.wantImage)
			}
			if tc.wantFirst && cs[0].Name != MainContainer {
				t.Errorf("main not first: %v", names(cs))
			}
			if want := []string{"sh", "-c", SleepScript}; !reflect.DeepEqual(m.Command, want) {
				t.Errorf("command = %q", m.Command)
			}
			if m.Args != nil {
				t.Errorf("args must be cleared, got %q", m.Args)
			}
			if m.WorkingDir != SharedMount {
				t.Errorf("workingDir = %q", m.WorkingDir)
			}
		})
	}
}

func TestMainAbsentWithoutImageIsError(t *testing.T) {
	_, err := BuildJob(BuildInput{Spec: mustSpec(t, "containers: [{name: dind, image: docker:dind}]"), EnvID: "e", Namespace: "n"})
	if err == nil {
		t.Fatal("want error when there is no main container and no job image")
	}
}

func TestPodspecNotMutated(t *testing.T) {
	s := mustSpec(t, plain)
	before := s.DeepCopy()
	build(t, BuildInput{Spec: s, Image: "alpine", Services: []Service{{Name: "db", Image: "postgres"}}})
	if !reflect.DeepEqual(s, before) {
		t.Error("BuildJob mutated its input podspec")
	}
}

func TestEnvKeepsPodspecEnvAndDoesNotPinPath(t *testing.T) {
	m := container(t, build(t, BuildInput{Spec: mustSpec(t, plain)}).Spec.Template.Spec.Containers, MainContainer)
	if len(m.Env) != 1 || m.Env[0].Name != "FOO" {
		t.Errorf("env = %v", m.Env)
	}
}

func TestJobSettings(t *testing.T) {
	j := build(t, BuildInput{Spec: mustSpec(t, plain), Timeout: 90 * time.Minute})
	ps := j.Spec.Template.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q", ps.RestartPolicy)
	}
	if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v", j.Spec.BackoffLimit)
	}
	if j.Spec.ActiveDeadlineSeconds == nil || *j.Spec.ActiveDeadlineSeconds != 5400 {
		t.Errorf("activeDeadlineSeconds = %v", j.Spec.ActiveDeadlineSeconds)
	}
	if j.Spec.TTLSecondsAfterFinished == nil || *j.Spec.TTLSecondsAfterFinished != 300 {
		t.Errorf("ttlSecondsAfterFinished = %v", j.Spec.TTLSecondsAfterFinished)
	}
	if j.Name != "gk-abc123" || j.Namespace != "ci" {
		t.Errorf("name/namespace = %s/%s", j.Namespace, j.Name)
	}
	if nt := build(t, BuildInput{Spec: mustSpec(t, plain)}); nt.Spec.ActiveDeadlineSeconds != nil {
		t.Error("zero timeout must leave activeDeadlineSeconds unset")
	}
}

func TestServices(t *testing.T) {
	res := &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}}
	j := build(t, BuildInput{
		Spec:             mustSpec(t, plain),
		PullPolicy:       corev1.PullAlways,
		ServiceResources: res,
		Services: []Service{
			{Name: "postgres", Image: "postgres:17", Env: map[string]string{"POSTGRES_PASSWORD": "pw", "A": "1"}, Ports: []string{"5432"}},
			{Name: "Web_App", Image: "nginx", Ports: []string{"8080:80", "127.0.0.1:9090:90", "53/udp"}},
		},
	})
	ps := j.Spec.Template.Spec
	pg := container(t, ps.InitContainers, "svc-postgres")
	if pg.RestartPolicy == nil || *pg.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("service must be a native sidecar, restartPolicy = %v", pg.RestartPolicy)
	}
	if pg.Image != "postgres:17" || pg.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("image/pull = %s %s", pg.Image, pg.ImagePullPolicy)
	}
	if want := []corev1.EnvVar{{Name: "A", Value: "1"}, {Name: "POSTGRES_PASSWORD", Value: "pw"}}; !reflect.DeepEqual(pg.Env, want) {
		t.Errorf("env = %v (want sorted)", pg.Env)
	}
	if want := []corev1.ContainerPort{{ContainerPort: 5432, Protocol: corev1.ProtocolTCP}}; !reflect.DeepEqual(pg.Ports, want) {
		t.Errorf("ports = %v", pg.Ports)
	}
	if !reflect.DeepEqual(pg.Resources, *res) {
		t.Errorf("resources = %v", pg.Resources)
	}
	web := container(t, ps.InitContainers, "svc-web-app")
	wantPorts := []corev1.ContainerPort{
		{ContainerPort: 80, Protocol: corev1.ProtocolTCP},
		{ContainerPort: 90, Protocol: corev1.ProtocolTCP},
		{ContainerPort: 53, Protocol: corev1.ProtocolUDP},
	}
	if !reflect.DeepEqual(web.Ports, wantPorts) {
		t.Errorf("ports = %v", web.Ports)
	}
	var aliases []string
	for _, h := range ps.HostAliases {
		if h.IP == "127.0.0.1" {
			aliases = append(aliases, h.Hostnames...)
		}
	}
	sort.Strings(aliases)
	if want := []string{"Web_App", "postgres"}; !reflect.DeepEqual(aliases, want) {
		t.Errorf("hostAliases = %v, want %v", aliases, want)
	}
}

func TestBadServicePort(t *testing.T) {
	_, err := BuildJob(BuildInput{Spec: mustSpec(t, plain), EnvID: "e", Namespace: "n",
		Services: []Service{{Name: "x", Image: "x", Ports: []string{"http"}}}})
	if err == nil {
		t.Fatal("want error for non-numeric port")
	}
}

func TestSharedVolume(t *testing.T) {
	t.Run("absent adds emptyDir 10Gi", func(t *testing.T) {
		ps := build(t, BuildInput{Spec: mustSpec(t, plain)}).Spec.Template.Spec
		v := volume(t, ps.Volumes, SharedVolume)
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(resource.MustParse("10Gi")) != 0 {
			t.Errorf("shared volume = %+v", v.VolumeSource)
		}
		assertMounted(t, container(t, ps.Containers, MainContainer))
	})
	t.Run("present is kept verbatim", func(t *testing.T) {
		spec := mustSpec(t, plain+`
volumes:
- name: shared
  ephemeral:
    volumeClaimTemplate:
      spec:
        accessModes: [ReadWriteOnce]
        resources: {requests: {storage: 20Gi}}
`)
		ps := build(t, BuildInput{Spec: spec}).Spec.Template.Spec
		if v := volume(t, ps.Volumes, SharedVolume); v.Ephemeral == nil || v.EmptyDir != nil {
			t.Errorf("shared volume replaced: %+v", v.VolumeSource)
		}
		if len(ps.Volumes) != 1 {
			t.Errorf("volumes = %d, want 1", len(ps.Volumes))
		}
		assertMounted(t, container(t, ps.Containers, MainContainer))
	})
	t.Run("existing mount not duplicated", func(t *testing.T) {
		spec := mustSpec(t, plain+"volumes: [{name: shared, emptyDir: {}}]\n")
		spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: SharedVolume, MountPath: SharedMount}}
		m := container(t, build(t, BuildInput{Spec: spec}).Spec.Template.Spec.Containers, MainContainer)
		if len(m.VolumeMounts) != 1 {
			t.Errorf("mounts = %v", m.VolumeMounts)
		}
	})
}

func volume(t *testing.T, vs []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for _, v := range vs {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("volume %q not found", name)
	return corev1.Volume{}
}

func assertMounted(t *testing.T, c corev1.Container) {
	t.Helper()
	for _, m := range c.VolumeMounts {
		if m.Name == SharedVolume && m.MountPath == SharedMount {
			return
		}
	}
	t.Errorf("%s does not mount %s at %s: %v", c.Name, SharedVolume, SharedMount, c.VolumeMounts)
}

func TestPassthrough(t *testing.T) {
	spec := mustSpec(t, plain+`
schedulerName: capacity-scheduler
priorityClassName: ci-low
imagePullSecrets: [{name: regcred}]
serviceAccountName: job-sa
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
      - matchExpressions: [{key: pool, operator: In, values: [ci]}]
initContainers:
- name: dind
  image: docker:dind
  restartPolicy: Always
  securityContext: {privileged: true}
`)
	ps := build(t, BuildInput{Spec: spec, Services: []Service{{Name: "db", Image: "postgres"}}}).Spec.Template.Spec
	if ps.SchedulerName != "capacity-scheduler" || ps.PriorityClassName != "ci-low" || ps.ServiceAccountName != "job-sa" {
		t.Errorf("scheduler/priority/sa = %q %q %q", ps.SchedulerName, ps.PriorityClassName, ps.ServiceAccountName)
	}
	if len(ps.ImagePullSecrets) != 1 || ps.Affinity == nil || ps.Affinity.NodeAffinity == nil {
		t.Error("imagePullSecrets/affinity dropped")
	}
	if got := names(ps.InitContainers); !reflect.DeepEqual(got, []string{"dind", "svc-db"}) {
		t.Errorf("initContainers = %v, want podspec sidecars first then services", got)
	}
	if !reflect.DeepEqual(ps.InitContainers[0], spec.InitContainers[0]) {
		t.Error("dind sidecar changed")
	}
}

func TestLabels(t *testing.T) {
	j := build(t, BuildInput{Spec: mustSpec(t, plain), Labels: map[string]string{"team": "ci", "env": "${ENV_ID}"}})
	want := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelInstance:  "runner-0",
		LabelEnvID:     "gk-abc123",
		"team":         "ci",
		"env":          "gk-abc123",
	}
	if !reflect.DeepEqual(j.Labels, want) {
		t.Errorf("job labels = %v", j.Labels)
	}
	if !reflect.DeepEqual(j.Spec.Template.Labels, want) {
		t.Errorf("pod labels = %v", j.Spec.Template.Labels)
	}
	if LabelManagedBy != "app.kubernetes.io/managed-by" || ManagedByValue != "gitea-k8s-runner-plugin" ||
		LabelInstance != "gitea-k8s-runner-plugin/instance" || LabelEnvID != "gitea-k8s-runner-plugin/environment-id" {
		t.Error("label keys drifted from the spec")
	}
}

func TestExtraLabelsCannotOverrideOwnership(t *testing.T) {
	j := build(t, BuildInput{Spec: mustSpec(t, plain), Labels: map[string]string{LabelInstance: "someone-else"}})
	if j.Labels[LabelInstance] != "runner-0" {
		t.Errorf("instance label overridden: %v", j.Labels)
	}
}

// The runner lays the workspace out under SharedMount, so a podspec mounting
// its own "shared" volume elsewhere in main is moved there, keeping the rest.
func TestSharedMountAtOtherPathIsMoved(t *testing.T) {
	spec := mustSpec(t, plain+"volumes: [{name: shared, emptyDir: {}}]\n")
	spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: SharedVolume, MountPath: "/work", SubPath: "ws"}}
	m := container(t, build(t, BuildInput{Spec: spec}).Spec.Template.Spec.Containers, MainContainer)
	want := []corev1.VolumeMount{{Name: SharedVolume, MountPath: SharedMount, SubPath: "ws"}}
	if !reflect.DeepEqual(m.VolumeMounts, want) {
		t.Errorf("mounts = %+v, want %+v", m.VolumeMounts, want)
	}
	if spec.Containers[0].VolumeMounts[0].MountPath != "/work" {
		t.Error("input podspec was mutated")
	}
}
