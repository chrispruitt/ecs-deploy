package internal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fakeSSM records the writes made to it. putErr/deleteErr, when set, are
// returned from the corresponding call.
type fakeSSM struct {
	puts      []*ssm.PutParameterInput
	deletes   []string
	putErr    error
	deleteErr error
}

func (f *fakeSSM) GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	return nil, errors.New("GetParameter not expected")
}

func (f *fakeSSM) PutParameter(ctx context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	// Like the real client, a cancelled context fails the call.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.puts = append(f.puts, in)
	return &ssm.PutParameterOutput{}, f.putErr
}

func (f *fakeSSM) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	f.deletes = append(f.deletes, aws.ToString(in.Name))
	return &ssm.DeleteParameterOutput{}, f.deleteErr
}

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

// A deployment that is certain not to be running the new tag must put the
// parameter back where it was, so the next infrastructure apply does not act on
// a tag that never shipped.
func TestRestoreVersionParameter(t *testing.T) {
	ctx := context.Background()

	t.Run("updated parameter is put back to its previous value", func(t *testing.T) {
		f := &fakeSSM{}
		v := &versionParameter{path: "/prod/svc/VERSION", from: "v0.4.0", to: "v0.4.1", existed: true}
		if err := restoreVersionParameter(ctx, f, v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(f.puts) != 1 || len(f.deletes) != 0 {
			t.Fatalf("want 1 put and 0 deletes, got %d puts, %d deletes", len(f.puts), len(f.deletes))
		}
		in := f.puts[0]
		if aws.ToString(in.Name) != "/prod/svc/VERSION" || aws.ToString(in.Value) != "v0.4.0" || !aws.ToBool(in.Overwrite) {
			t.Errorf("unexpected put: name=%q value=%q overwrite=%v", aws.ToString(in.Name), aws.ToString(in.Value), aws.ToBool(in.Overwrite))
		}
		// Sending a Type on overwrite would be rejected if it disagreed with the
		// existing parameter's type.
		if in.Type != "" {
			t.Errorf("restore must not send a Type, got %q", in.Type)
		}
	})

	t.Run("created parameter is deleted", func(t *testing.T) {
		f := &fakeSSM{}
		v := &versionParameter{path: "/prod/svc/VERSION", to: "v0.4.1"}
		if err := restoreVersionParameter(ctx, f, v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(f.deletes) != 1 || f.deletes[0] != "/prod/svc/VERSION" || len(f.puts) != 0 {
			t.Errorf("want a single delete of the parameter, got puts=%d deletes=%v", len(f.puts), f.deletes)
		}
	})

	t.Run("already-deleted parameter is not an error", func(t *testing.T) {
		f := &fakeSSM{deleteErr: &ssmtypes.ParameterNotFound{}}
		v := &versionParameter{path: "/prod/svc/VERSION", to: "v0.4.1"}
		if err := restoreVersionParameter(ctx, f, v); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("nothing written means nothing restored", func(t *testing.T) {
		f := &fakeSSM{}
		unchanged := &versionParameter{path: "/p/svc/VERSION", from: "v1", to: "v1", existed: true}
		for _, v := range []*versionParameter{nil, unchanged} {
			if err := restoreVersionParameter(ctx, f, v); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}
		if len(f.puts)+len(f.deletes) != 0 {
			t.Errorf("expected no SSM writes, got puts=%d deletes=%d", len(f.puts), len(f.deletes))
		}
	})

	t.Run("failed restore is reported", func(t *testing.T) {
		f := &fakeSSM{putErr: errors.New("AccessDenied")}
		v := &versionParameter{path: "/prod/svc/VERSION", from: "v0.4.0", to: "v0.4.1", existed: true}
		if err := restoreVersionParameter(ctx, f, v); err == nil {
			t.Error("expected the restore failure to be returned")
		}
	})
}

// The deploy error must reach the caller unchanged whether or not the restore
// works; a failed restore falls back to the manual guidance.
func TestWithVersionRollback(t *testing.T) {
	deployErr := errors.New("deployment FAILED")
	v := &versionParameter{path: "/prod/svc/VERSION", from: "v0.4.0", to: "v0.4.1", existed: true}

	for _, f := range []*fakeSSM{{}, {putErr: errors.New("AccessDenied")}} {
		if err := withVersionRollback(context.Background(), f, deployErr, v); err != deployErr {
			t.Errorf("want original error returned, got %v", err)
		}
	}

	// A cancelled context (Ctrl-C at the moment of failure) must not stop the restore.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeSSM{}
	_ = withVersionRollback(ctx, f, deployErr, v)
	if len(f.puts) != 1 {
		t.Errorf("restore should run on a cancelled context, got %d puts", len(f.puts))
	}
}
