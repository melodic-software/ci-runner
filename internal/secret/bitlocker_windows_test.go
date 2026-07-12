//go:build windows

package secret

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestEncodePowerShellUsesWindowsEncodedCommandFormat(t *testing.T) {
	t.Parallel()
	script := `$v=Get-BitLockerVolume -MountPoint 'C:'; "snowman: ☃"`
	raw, err := base64.StdEncoding.DecodeString(encodePowerShell(script))
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("encoded command has odd UTF-16LE byte count: %d", len(raw))
	}
	units := make([]uint16, len(raw)/2)
	for index := range units {
		units[index] = uint16(raw[index*2]) | uint16(raw[index*2+1])<<8
	}
	if got := string(utf16.Decode(units)); got != script {
		t.Fatalf("decoded script = %q, want %q", got, script)
	}
}

func TestElevatedBitLockerScriptUsesOnlyStatusAndExitCodes(t *testing.T) {
	t.Parallel()
	script := elevatedBitLockerScript(`C:`)
	for _, required := range []string{
		`$ProgressPreference='SilentlyContinue'`,
		`$WarningPreference='SilentlyContinue'`,
		"Get-BitLockerVolume -MountPoint 'C:'",
		`VolumeStatus`,
		`ProtectionStatus`,
		`exit 10`,
		`exit 11`,
		`exit 12`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("script does not contain %q: %s", required, script)
		}
	}
	for _, forbidden := range []string{
		"PRIVATE KEY", "CryptProtectData", "Ciphertext", "DPAPI", "WriteAllText", "TEMP", "result.json",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("elevated query unexpectedly contains %q", forbidden)
		}
	}
}

func TestMachineReadablePowerShellPathsSuppressProgressAndForceText(t *testing.T) {
	t.Parallel()
	childArguments := strings.Join(powerShellArguments(elevatedBitLockerScript(`C:`)), " ")
	for name, script := range map[string]string{
		"direct":   bitLockerQueryScript(`C:`),
		"elevated": elevatedBitLockerScript(`C:`),
		"launcher": elevatedBitLockerLauncherScript(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, childArguments),
	} {
		for _, preference := range []string{
			`$ProgressPreference='SilentlyContinue'`,
			`$WarningPreference='SilentlyContinue'`,
			`$InformationPreference='SilentlyContinue'`,
			`$VerbosePreference='SilentlyContinue'`,
			`$DebugPreference='SilentlyContinue'`,
		} {
			if !strings.Contains(script, preference) {
				t.Fatalf("%s script does not contain %s: %s", name, preference, script)
			}
		}
	}
	arguments := powerShellArguments(`Write-Output fixture`)
	if !slices.Contains(arguments, "-OutputFormat") || !slices.Contains(arguments, "Text") {
		t.Fatalf("PowerShell arguments do not force text output: %q", arguments)
	}
	if !strings.Contains(childArguments, "-OutputFormat Text") {
		t.Fatalf("elevated child arguments do not force text output: %q", childArguments)
	}
}

func TestRunPowerShellSuppressesModuleProgressInMachineReadableOutput(t *testing.T) {
	powerShell, err := windowsPowerShellPath()
	if err != nil {
		t.Fatalf("resolve Windows PowerShell: %v", err)
	}
	script := machineReadablePowerShellPrefix() + `Import-Module BitLocker -ErrorAction Stop;[Console]::Out.Write('fixture')`
	out, err := runPowerShell(context.Background(), powerShell, script)
	if err != nil {
		t.Fatalf("run Windows PowerShell: %v (%s)", err, out)
	}
	if got, want := string(out), "fixture"; got != want {
		t.Fatalf("machine-readable output = %q, want %q", got, want)
	}
}

func TestElevatedBitLockerExitCodeContract(t *testing.T) {
	t.Parallel()
	if err := parseElevatedBitLockerExit([]byte("0")); err != nil {
		t.Fatalf("protected status: %v", err)
	}
	for value, expected := range map[string]string{
		"10": "not FullyEncrypted",
		"11": "not On",
		"12": "failed",
		"99": "unsupported exit code",
		"x":  "parse",
	} {
		if err := parseElevatedBitLockerExit([]byte(value)); err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatalf("exit %q error = %v, want substring %q", value, err, expected)
		}
	}
}

func TestWindowsPowerShellPathIgnoresForgedSystemRoot(t *testing.T) {
	forged := t.TempDir()
	t.Setenv("SystemRoot", forged)
	got, err := windowsPowerShellPath()
	if err != nil {
		t.Fatalf("windowsPowerShellPath: %v", err)
	}
	systemDirectory, err := getSystemDirectory()
	if err != nil {
		t.Fatalf("getSystemDirectory: %v", err)
	}
	want := filepath.Join(systemDirectory, "WindowsPowerShell", "v1.0", "powershell.exe")
	if !strings.EqualFold(got, want) {
		t.Fatalf("windowsPowerShellPath = %q, want trusted system path %q", got, want)
	}
	if strings.HasPrefix(strings.ToLower(got), strings.ToLower(forged)) {
		t.Fatalf("trusted PowerShell path used forged SystemRoot: %q", got)
	}
}

func TestQuotePowerShellLiteralDoublesApostrophes(t *testing.T) {
	t.Parallel()
	if got, want := quotePowerShellLiteral(`C:\O'Brien`), `C:\O''Brien`; got != want {
		t.Fatalf("quotePowerShellLiteral() = %q, want %q", got, want)
	}
}
