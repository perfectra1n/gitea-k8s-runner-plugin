package pluginv1_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"

	pluginv1 "github.com/perfectra1n/gitea-k8s-runner-plugin/gen/plugin/v1alpha"
)

// The field numbers below are fixed by wire compatibility with Forgejo's
// plugin.v1alpha protocol. A renumbering compiles fine and breaks interop, so
// pin the encoded tag bytes (field<<3 | wiretype).
func TestFieldTags(t *testing.T) {
	cases := []struct {
		name string
		msg  proto.Message
		want []byte
	}{
		{"CreateRequest.label_arg=9", &pluginv1.CreateRequest{LabelArg: "x"}, []byte{tag(9, wireBytes), 1, 'x'}},
		{"CreateRequest.environment_timeout=10", &pluginv1.CreateRequest{EnvironmentTimeout: durationpb.New(0)}, []byte{tag(10, wireBytes), 0}},
		{"CreateResponse.os=10", &pluginv1.CreateResponse{Os: "l"}, []byte{tag(10, wireBytes), 1, 'l'}},
		{"CreateResponse.arch=11", &pluginv1.CreateResponse{Arch: "a"}, []byte{tag(11, wireBytes), 1, 'a'}},
		{"CopyInChunk.data=3", &pluginv1.CopyInChunk{Data: []byte{7}}, []byte{tag(3, wireBytes), 1, 7}},
		{"CopyInChunk.dest_path=2", &pluginv1.CopyInChunk{DestPath: proto.String("/")}, []byte{tag(2, wireBytes), 1, '/'}},
		{"ExecComplete.exit_code=1", &pluginv1.ExecComplete{ExitCode: 3}, []byte{tag(1, wireVarint), 3}},
		{"ExecFailed.error_message=1", &pluginv1.ExecFailed{ErrorMessage: "e"}, []byte{tag(1, wireBytes), 1, 'e'}},
		{"ExecOutput.exec_failed=3", &pluginv1.ExecOutput{Output: &pluginv1.ExecOutput_ExecFailed{ExecFailed: &pluginv1.ExecFailed{}}}, []byte{tag(3, wireBytes), 0}},
		{"ExecRequest.workdir=5", &pluginv1.ExecRequest{Workdir: "w"}, []byte{tag(5, wireBytes), 1, 'w'}},
		{"StartOutput.start_complete=2", &pluginv1.StartOutput{Output: &pluginv1.StartOutput_StartComplete{StartComplete: &pluginv1.StartComplete{}}}, []byte{tag(2, wireBytes), 0}},
		{"CapabilitiesResponse.name=1", &pluginv1.CapabilitiesResponse{Name: "k"}, []byte{tag(1, wireBytes), 1, 'k'}},
		{"ServiceContainer.ports=4", &pluginv1.ServiceContainer{Ports: []string{"p"}}, []byte{tag(4, wireBytes), 1, 'p'}},
		{"DataChunk.stream=1 STDERR=1", &pluginv1.DataChunk{Stream: pluginv1.DataChunk_STDERR}, []byte{tag(1, wireVarint), 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := proto.MarshalOptions{Deterministic: true}.Marshal(tc.msg)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("encoded %x, want %x", got, tc.want)
			}
		})
	}
}

func TestReservedFieldsAreUnknown(t *testing.T) {
	// CreateRequest reserves 3 and 4; CapabilitiesResponse reserves 2..7.
	for _, n := range []int{3, 4} {
		if pluginv1.File_plugin_v1alpha_plugin_proto.Messages().ByName("CreateRequest").Fields().ByNumber(protoNum(n)) != nil {
			t.Errorf("CreateRequest field %d must stay reserved", n)
		}
	}
	for n := 2; n <= 7; n++ {
		if pluginv1.File_plugin_v1alpha_plugin_proto.Messages().ByName("CapabilitiesResponse").Fields().ByNumber(protoNum(n)) != nil {
			t.Errorf("CapabilitiesResponse field %d must stay reserved", n)
		}
	}
}

func TestFullMethodNames(t *testing.T) {
	want := map[string]string{
		pluginv1.BackendPlugin_Capabilities_FullMethodName: "Capabilities",
		pluginv1.BackendPlugin_Create_FullMethodName:       "Create",
		pluginv1.BackendPlugin_Start_FullMethodName:        "Start",
		pluginv1.BackendPlugin_Exec_FullMethodName:         "Exec",
		pluginv1.BackendPlugin_CopyIn_FullMethodName:       "CopyIn",
		pluginv1.BackendPlugin_CopyOut_FullMethodName:      "CopyOut",
		pluginv1.BackendPlugin_Remove_FullMethodName:       "Remove",
	}
	for got, rpc := range want {
		if exp := "/plugin.v1alpha.BackendPlugin/" + rpc; got != exp {
			t.Errorf("full method %q, want %q", got, exp)
		}
	}
}

func protoNum(n int) protoreflect.FieldNumber { return protoreflect.FieldNumber(n) }

const (
	wireVarint = 0
	wireBytes  = 2
)

// tag is the single-byte key (field<<3 | wiretype) for small field numbers.
func tag(field, wire byte) byte { return field<<3 | wire }
