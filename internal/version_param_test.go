package internal

import (
	"strings"
	"testing"
)

func TestVersionParameterChanged(t *testing.T) {
	tests := []struct {
		name string
		v    versionParameter
		want bool
	}{
		{
			name: "not yet created is a change",
			v:    versionParameter{path: "/p/svc/VERSION", to: "v1.0.0"},
			want: true,
		},
		{
			name: "different value is a change",
			v:    versionParameter{path: "/p/svc/VERSION", from: "v1.0.0", to: "v1.0.1", existed: true},
			want: true,
		},
		{
			name: "same value is not a change",
			v:    versionParameter{path: "/p/svc/VERSION", from: "v1.0.0", to: "v1.0.0", existed: true},
			want: false,
		},
		{
			name: "redeploying the same tag is not a change",
			v:    versionParameter{path: "/p/svc/VERSION", from: "latest", to: "latest", existed: true},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.changed(); got != tt.want {
				t.Errorf("changed() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A deployment that fails after the parameter is written leaves it naming a tag
// that is not running. The whole point of the parameter is that such a
// disagreement is invisible, so the guidance must name the previous value and be
// actionable — not just say "something went wrong".
func TestDescribeVersionRollback(t *testing.T) {
	t.Run("updated parameter names the value to restore", func(t *testing.T) {
		msg := describeVersionRollback(&versionParameter{
			path: "/prod/chuckhires-ui/VERSION", from: "v0.4.0", to: "v0.4.1", existed: true,
		})
		for _, want := range []string{"/prod/chuckhires-ui/VERSION", "v0.4.1", "v0.4.0", "put-parameter"} {
			if !strings.Contains(msg, want) {
				t.Errorf("rollback guidance missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("created parameter says to delete it", func(t *testing.T) {
		msg := describeVersionRollback(&versionParameter{
			path: "/prod/chuckhires-ui/VERSION", to: "v0.4.1",
		})
		if !strings.Contains(msg, "delete-parameter") {
			t.Errorf("expected delete guidance for a parameter that did not exist before:\n%s", msg)
		}
		// There is no previous value to restore, so it must not invent one.
		if strings.Contains(msg, "put-parameter") {
			t.Errorf("must not suggest restoring a value that never existed:\n%s", msg)
		}
	})

	t.Run("silent when nothing was written", func(t *testing.T) {
		if msg := describeVersionRollback(nil); msg != "" {
			t.Errorf("no parameter configured should produce no warning, got:\n%s", msg)
		}
		unchanged := &versionParameter{path: "/p/svc/VERSION", from: "v1", to: "v1", existed: true}
		if msg := describeVersionRollback(unchanged); msg != "" {
			t.Errorf("unwritten parameter should produce no warning, got:\n%s", msg)
		}
	})
}
