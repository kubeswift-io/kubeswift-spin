package translate

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	spinv1alpha1 "github.com/spinkube/spin-operator/api/v1alpha1"
)

func res(limits, requests map[corev1.ResourceName]string) spinv1alpha1.Resources {
	r := spinv1alpha1.Resources{}
	if limits != nil {
		r.Limits = corev1.ResourceList{}
		for k, v := range limits {
			r.Limits[k] = resource.MustParse(v)
		}
	}
	if requests != nil {
		r.Requests = corev1.ResourceList{}
		for k, v := range requests {
			r.Requests[k] = resource.MustParse(v)
		}
	}
	return r
}

func TestVCPUs(t *testing.T) {
	policy := DefaultResourcePolicy()
	def := resource.MustParse("1")
	cpu := corev1.ResourceCPU
	tests := []struct {
		name    string
		res     spinv1alpha1.Resources
		def     resource.Quantity
		want    int32
		wantErr string
	}{
		{"default when unset", res(nil, nil), def, 1, ""},
		{"default of 2", res(nil, nil), resource.MustParse("2"), 2, ""},
		{"1m rounds up", res(map[corev1.ResourceName]string{cpu: "1m"}, nil), def, 1, ""},
		{"100m rounds up", res(map[corev1.ResourceName]string{cpu: "100m"}, nil), def, 1, ""},
		{"250m rounds up", res(map[corev1.ResourceName]string{cpu: "250m"}, nil), def, 1, ""},
		{"500m rounds up", res(map[corev1.ResourceName]string{cpu: "500m"}, nil), def, 1, ""},
		{"999m rounds up", res(map[corev1.ResourceName]string{cpu: "999m"}, nil), def, 1, ""},
		{"exactly 1", res(map[corev1.ResourceName]string{cpu: "1"}, nil), def, 1, ""},
		{"1000m is 1", res(map[corev1.ResourceName]string{cpu: "1000m"}, nil), def, 1, ""},
		{"1001m is 2", res(map[corev1.ResourceName]string{cpu: "1001m"}, nil), def, 2, ""},
		{"1500m is 2", res(map[corev1.ResourceName]string{cpu: "1500m"}, nil), def, 2, ""},
		{"2", res(map[corev1.ResourceName]string{cpu: "2"}, nil), def, 2, ""},
		{"2.5 is 3", res(map[corev1.ResourceName]string{cpu: "2.5"}, nil), def, 3, ""},
		{"max allowed", res(map[corev1.ResourceName]string{cpu: "8"}, nil), def, 8, ""},
		{"limit wins over request", res(map[corev1.ResourceName]string{cpu: "2"}, map[corev1.ResourceName]string{cpu: "500m"}), def, 2, ""},
		{"request used without limit", res(nil, map[corev1.ResourceName]string{cpu: "1500m"}), def, 2, ""},
		{"request above limit", res(map[corev1.ResourceName]string{cpu: "1"}, map[corev1.ResourceName]string{cpu: "2"}), def, 0, "exceeds"},
		{"zero limit", res(map[corev1.ResourceName]string{cpu: "0"}, nil), def, 0, "greater than zero"},
		{"zero default", res(nil, nil), resource.MustParse("0"), 0, "greater than zero"},
		{"negative", res(map[corev1.ResourceName]string{cpu: "-1"}, nil), def, 0, "greater than zero"},
		{"above max", res(map[corev1.ResourceName]string{cpu: "9"}, nil), def, 0, "maximum of 8"},
		{"rounds above max", res(map[corev1.ResourceName]string{cpu: "8001m"}, nil), def, 0, "maximum of 8"},
		{"absurd value", res(map[corev1.ResourceName]string{cpu: "1e12"}, nil), def, 0, "out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := VCPUs(tt.res, tt.def, policy)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v (value %d)", tt.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %d vCPUs, want %d", got, tt.want)
			}
		})
	}
}

func TestMemory(t *testing.T) {
	policy := DefaultResourcePolicy()
	def := resource.MustParse("512Mi")
	mem := corev1.ResourceMemory
	tests := []struct {
		name    string
		res     spinv1alpha1.Resources
		def     resource.Quantity
		want    string
		wantErr string
	}{
		{"default when unset", res(nil, nil), def, "512Mi", ""},
		{"binary Mi unchanged", res(map[corev1.ResourceName]string{mem: "256Mi"}, nil), def, "256Mi", ""},
		{"binary Gi to Mi", res(map[corev1.ResourceName]string{mem: "1Gi"}, nil), def, "1Gi", ""},
		{"decimal G rounds up to Mi", res(map[corev1.ResourceName]string{mem: "1G"}, nil), def, "954Mi", ""},
		{"decimal M rounds up to Mi", res(map[corev1.ResourceName]string{mem: "300M"}, nil), def, "287Mi", ""},
		{"plain bytes", res(map[corev1.ResourceName]string{mem: "268435456"}, nil), def, "256Mi", ""},
		{"one byte over rounds up", res(map[corev1.ResourceName]string{mem: "268435457"}, nil), def, "257Mi", ""},
		{"fractional Gi", res(map[corev1.ResourceName]string{mem: "1.5Gi"}, nil), def, "1536Mi", ""},
		{"Ki rounds up", res(map[corev1.ResourceName]string{mem: "300000Ki"}, nil), def, "293Mi", ""},
		{"limit wins", res(map[corev1.ResourceName]string{mem: "1Gi"}, map[corev1.ResourceName]string{mem: "512Mi"}), def, "1Gi", ""},
		{"request used without limit", res(nil, map[corev1.ResourceName]string{mem: "768Mi"}), def, "768Mi", ""},
		{"request above limit", res(map[corev1.ResourceName]string{mem: "512Mi"}, map[corev1.ResourceName]string{mem: "1Gi"}), def, "", "exceeds"},
		{"below minimum is rejected, not raised", res(map[corev1.ResourceName]string{mem: "128Mi"}, nil), def, "", "below the minimum of 256Mi"},
		{"decimal just below minimum", res(map[corev1.ResourceName]string{mem: "256M"}, nil), def, "", "below the minimum"},
		{"maximum accepted", res(map[corev1.ResourceName]string{mem: "16Gi"}, nil), def, "16Gi", ""},
		{"above maximum", res(map[corev1.ResourceName]string{mem: "17Gi"}, nil), def, "", "above the maximum"},
		{"zero", res(map[corev1.ResourceName]string{mem: "0"}, nil), def, "", "greater than zero"},
		{"negative", res(map[corev1.ResourceName]string{mem: "-1Gi"}, nil), def, "", "greater than zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Memory(tt.res, tt.def, policy)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := resource.MustParse(tt.want)
			if got.Cmp(want) != 0 {
				t.Fatalf("got %s, want %s", got.String(), tt.want)
			}
			if got.Value()%mebibyte != 0 {
				t.Fatalf("result %s is not a whole MiB", got.String())
			}
		})
	}
}

func TestMemoryNeverBelowRequest(t *testing.T) {
	policy := ResourcePolicy{}
	for _, v := range []string{"1", "1000", "1048575", "1048577", "1M", "999999999", "3G", "2.25Gi"} {
		q := resource.MustParse(v)
		got, err := Memory(spinv1alpha1.Resources{Limits: corev1.ResourceList{corev1.ResourceMemory: q}}, q, policy)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if got.Cmp(q) < 0 {
			t.Fatalf("%s converted to %s, which is smaller", v, got.String())
		}
		if got.Value()-q.Value() >= mebibyte {
			t.Fatalf("%s converted to %s, rounded up by a full MiB or more", v, got.String())
		}
	}
}
