//go:build windows

package host

import (
	"context"
	"reflect"
	"testing"
)

type recordingCommandRunner struct {
	name string
	args []string
	out  []byte
	err  error
}

func (r *recordingCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.out, r.err
}

func TestDockerInspectorPinsLocalEngineHost(t *testing.T) {
	t.Parallel()
	executable, err := trustedDockerDesktopExecutable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		run  func(DockerCLIInspector) error
		want []string
	}{
		{
			name: "info",
			run: func(inspector DockerCLIInspector) error {
				_, err := inspector.EngineReachable(context.Background())
				return err
			},
			want: []string{"--host", localDockerEngineHost, "info", "--format", "{{json .ServerVersion}}"},
		},
		{
			name: "containers",
			run: func(inspector DockerCLIInspector) error {
				_, err := inspector.Containers(context.Background())
				return err
			},
			want: []string{"--host", localDockerEngineHost, "ps", "--format", "{{json .}}"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner := &recordingCommandRunner{}
			if err := test.run(DockerCLIInspector{Runner: runner}); err != nil {
				t.Fatal(err)
			}
			if runner.name != executable || !reflect.DeepEqual(runner.args, test.want) {
				t.Fatalf("command = %q %#v, want %q %#v", runner.name, runner.args, executable, test.want)
			}
		})
	}
}
