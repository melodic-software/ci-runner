// Package seccomp holds the named seccomp profiles a target may select for its
// workers. Only these embedded profiles exist; a host config cannot name a path.
package seccomp

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
)

// UserNS is the moby default profile plus two rules that let a process without
// CAP_SYS_ADMIN call clone and unshare when the flags include CLONE_NEWUSER.
const UserNS = "userns"

// MobyDefaultBlobSHA is the git blob SHA of moby-default.json, which is
// github.com/moby/profiles seccomp/default.json at tag seccomp/v0.2.3, the
// version Docker Engine v29.8.2 vendors as its built-in profile.
const MobyDefaultBlobSHA = "ea5a494afb8d64898fa0f4f47ae0c4f5ba9cbbc9"

//go:embed moby-default.json
var mobyDefault []byte

const cloneNewUser = 0x10000000

// userNSRules are inserted directly after the default's own non-CAP_SYS_ADMIN
// clone rules. SCMP_CMP_MASKED_EQ with value and valueTwo both CLONE_NEWUSER
// matches when (flags & CLONE_NEWUSER) == CLONE_NEWUSER. The kernel still
// decides what the new user namespace may do.
var userNSRules = []map[string]any{
	{
		"names":    []string{"clone"},
		"action":   "SCMP_ACT_ALLOW",
		"args":     []map[string]any{{"index": 0, "value": cloneNewUser, "valueTwo": cloneNewUser, "op": "SCMP_CMP_MASKED_EQ"}},
		"comment":  "ci-runner userns: clone with CLONE_NEWUSER",
		"excludes": map[string]any{"caps": []string{"CAP_SYS_ADMIN"}, "arches": []string{"s390", "s390x"}},
	},
	{
		"names":    []string{"unshare"},
		"action":   "SCMP_ACT_ALLOW",
		"args":     []map[string]any{{"index": 0, "value": cloneNewUser, "valueTwo": cloneNewUser, "op": "SCMP_CMP_MASKED_EQ"}},
		"comment":  "ci-runner userns: unshare with CLONE_NEWUSER",
		"excludes": map[string]any{"caps": []string{"CAP_SYS_ADMIN"}},
	},
}

// Names lists every profile a target may select.
func Names() []string { return []string{UserNS} }

// Valid reports whether name is a known profile.
func Valid(name string) bool { return slices.Contains(Names(), name) }

// Profile returns the compact JSON for a named profile.
func Profile(name string) ([]byte, error) {
	if name != UserNS {
		return nil, fmt.Errorf("unknown seccomp profile %q", name)
	}
	return userNSProfile()
}

func userNSProfile() ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(mobyDefault, &document); err != nil {
		return nil, fmt.Errorf("decode embedded moby seccomp profile: %w", err)
	}
	var syscalls []json.RawMessage
	if err := json.Unmarshal(document["syscalls"], &syscalls); err != nil {
		return nil, fmt.Errorf("decode embedded moby seccomp syscalls: %w", err)
	}
	insertAt := -1
	for index, raw := range syscalls {
		var rule struct {
			Names []string `json:"names"`
			Args  []any    `json:"args"`
		}
		if err := json.Unmarshal(raw, &rule); err != nil {
			return nil, fmt.Errorf("decode embedded moby seccomp rule %d: %w", index, err)
		}
		if slices.Equal(rule.Names, []string{"clone"}) && len(rule.Args) > 0 {
			insertAt = index + 1
		}
	}
	if insertAt < 0 {
		return nil, fmt.Errorf("embedded moby seccomp profile has no conditional clone rule")
	}
	added := make([]json.RawMessage, 0, len(userNSRules))
	for _, rule := range userNSRules {
		raw, err := json.Marshal(rule)
		if err != nil {
			return nil, err
		}
		added = append(added, raw)
	}
	syscalls = slices.Insert(syscalls, insertAt, added...)
	encoded, err := json.Marshal(syscalls)
	if err != nil {
		return nil, err
	}
	document["syscalls"] = encoded
	return json.Marshal(document)
}
