//go:build windows

package host

import (
	"context"
	"reflect"
	"testing"
)

func TestScheduledTaskStartUsesCanonicalNativeRunCommand(t *testing.T) {
	t.Parallel()
	runner := &recordingCommandRunner{}
	const executable = `C:\Windows\System32\schtasks.exe`
	task := ScheduledTaskCLI{Runner: runner, executablePath: executable}

	if err := task.Start(context.Background(), "ci-runner-fleet"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/Run", "/TN", "ci-runner-fleet"}
	if runner.name != executable || !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("command = %q %#v, want %q %#v", runner.name, runner.args, executable, want)
	}
}

func TestScheduledTaskStartRejectsEmptyNameBeforeSpawning(t *testing.T) {
	t.Parallel()
	runner := &recordingCommandRunner{}
	task := ScheduledTaskCLI{Runner: runner, executablePath: `C:\Windows\System32\schtasks.exe`}

	if err := task.Start(context.Background(), ""); err == nil {
		t.Fatal("empty task name was accepted")
	}
	if runner.name != "" || len(runner.args) != 0 {
		t.Fatalf("invalid task name spawned %q %#v", runner.name, runner.args)
	}
}
