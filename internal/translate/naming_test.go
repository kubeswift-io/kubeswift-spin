package translate

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestSandboxNameShortNamesAreReadable(t *testing.T) {
	for ord, want := range []string{"hello-0", "hello-1", "hello-2"} {
		if got := SandboxName("hello", ord); got != want {
			t.Fatalf("SandboxName(hello, %d) = %q, want %q", ord, got, want)
		}
	}
}

func TestSandboxNameIsAlwaysAValidLabel(t *testing.T) {
	names := []string{
		"a",
		"hello",
		"my.app.with.dots",
		strings.Repeat("x", 58),
		strings.Repeat("y", 59),
		strings.Repeat("z", 253),
		strings.Repeat("a.", 100) + "b",
		"-weird-",
		"...",
	}
	for _, n := range names {
		for _, ord := range []int{0, 7, 99, 9999} {
			got := SandboxName(n, ord)
			if len(got) > 63 {
				t.Fatalf("SandboxName(%q, %d) = %q has %d characters", n, ord, got, len(got))
			}
			if errs := validation.IsDNS1123Label(got); len(errs) > 0 {
				t.Fatalf("SandboxName(%q, %d) = %q is not a DNS label: %v", n, ord, got, errs)
			}
			if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
				t.Fatalf("SandboxName(%q, %d) = %q is not a label value: %v", n, ord, got, errs)
			}
		}
		if errs := validation.IsValidLabelValue(AppLabelValue(n)); len(errs) > 0 {
			t.Fatalf("AppLabelValue(%q) invalid: %v", n, errs)
		}
	}
}

func TestSandboxNameIsDeterministicAndCollisionFree(t *testing.T) {
	long := strings.Repeat("service", 20)
	a, b := long+"-a", long+"-b"
	if SandboxName(a, 0) == SandboxName(b, 0) {
		t.Fatalf("distinct long names collided: %s", SandboxName(a, 0))
	}
	if SandboxName("my.app", 0) == SandboxName("my-app", 0) {
		t.Fatalf("my.app and my-app collided")
	}
	if SandboxName(a, 3) != SandboxName(a, 3) {
		t.Fatalf("not deterministic")
	}
}

func TestParseOrdinal(t *testing.T) {
	if n, err := ParseOrdinal("12"); err != nil || n != 12 {
		t.Fatalf("got %d, %v", n, err)
	}
	for _, bad := range []string{"", "-1", "x", "1.5"} {
		if _, err := ParseOrdinal(bad); err == nil {
			t.Fatalf("ParseOrdinal(%q) accepted", bad)
		}
	}
}
