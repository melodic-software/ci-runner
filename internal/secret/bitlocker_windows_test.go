//go:build windows

package secret

import (
	"encoding/base64"
	"path/filepath"
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
