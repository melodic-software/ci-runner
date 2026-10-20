package seccomp

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestEmbeddedDefaultIsThePinnedUpstreamBlob(t *testing.T) {
	t.Parallel()
	sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(mobyDefault))), mobyDefault...))
	if got := fmt.Sprintf("%x", sum); got != MobyDefaultBlobSHA {
		t.Fatalf("moby-default.json blob = %s, want %s", got, MobyDefaultBlobSHA)
	}
}

func TestUserNSProfileIsTheDefaultPlusOnlyTheThreeUserNamespaceRules(t *testing.T) {
	t.Parallel()
	profile, err := Profile(UserNS)
	if err != nil {
		t.Fatal(err)
	}
	var base, derived map[string]any
	if err := json.Unmarshal(mobyDefault, &base); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(profile, &derived); err != nil {
		t.Fatal(err)
	}
	baseRules := base["syscalls"].([]any)
	derivedRules := derived["syscalls"].([]any)
	delete(base, "syscalls")
	delete(derived, "syscalls")
	if !reflect.DeepEqual(base, derived) {
		t.Fatal("userns profile changed a top-level field of the moby default")
	}
	if len(derivedRules) != len(baseRules)+3 {
		t.Fatalf("userns profile has %d rules, want default %d plus 3", len(derivedRules), len(baseRules))
	}
	var added []any
	for baseIndex, derivedIndex := 0, 0; derivedIndex < len(derivedRules); derivedIndex++ {
		if baseIndex < len(baseRules) && reflect.DeepEqual(baseRules[baseIndex], derivedRules[derivedIndex]) {
			baseIndex++
			continue
		}
		added = append(added, derivedRules[derivedIndex])
	}
	want := []any{
		map[string]any{
			"names": []any{"clone"}, "action": "SCMP_ACT_ALLOW",
			"args":     []any{map[string]any{"index": 0.0, "value": 268435456.0, "valueTwo": 268435456.0, "op": "SCMP_CMP_MASKED_EQ"}},
			"comment":  "ci-runner userns: clone with CLONE_NEWUSER",
			"excludes": map[string]any{"caps": []any{"CAP_SYS_ADMIN"}, "arches": []any{"s390", "s390x"}},
		},
		map[string]any{
			"names": []any{"unshare"}, "action": "SCMP_ACT_ALLOW",
			"args":     []any{map[string]any{"index": 0.0, "value": 268435456.0, "valueTwo": 268435456.0, "op": "SCMP_CMP_MASKED_EQ"}},
			"comment":  "ci-runner userns: unshare with CLONE_NEWUSER",
			"excludes": map[string]any{"caps": []any{"CAP_SYS_ADMIN"}},
		},
		map[string]any{
			"names": []any{"mount", "umount2", "pivot_root"}, "action": "SCMP_ACT_ALLOW",
			"comment":  "ci-runner userns: mount, umount2 and pivot_root for bubblewrap",
			"excludes": map[string]any{"caps": []any{"CAP_SYS_ADMIN"}},
		},
	}
	if !reflect.DeepEqual(added, want) {
		t.Fatalf("added rules = %#v, want %#v", added, want)
	}
}

func TestUnknownProfileIsRejected(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "unconfined", "default", "/etc/seccomp.json"} {
		if Valid(name) {
			t.Errorf("Valid(%q) = true", name)
		}
		if _, err := Profile(name); err == nil {
			t.Errorf("Profile(%q) returned no error", name)
		}
	}
}
