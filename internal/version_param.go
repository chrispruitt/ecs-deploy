package internal

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// versionParameter is the SSM parameter that records which image tag a service
// is running, so infrastructure-as-code can re-derive the deployed tag instead
// of reverting the service to whatever its configuration hardcodes.
//
// It is deliberately written BEFORE the service is updated. The parameter is
// commonly also injected into the container (as a secret or env var), and ECS
// resolves those at task start — a value written after the rollout leaves the
// new task reporting the version it replaced.
type versionParameter struct {
	path    string
	from    string // current value; empty when the parameter does not exist yet
	to      string // the tag being deployed
	existed bool
}

// changed reports whether the write would alter the stored value.
func (v *versionParameter) changed() bool {
	return !v.existed || v.from != v.to
}

// readVersionParameter loads the current value. A parameter that does not exist
// yet is not an error: it is created by the write that follows.
func readVersionParameter(ctx context.Context, client *ssm.Client, path, tag string) (*versionParameter, error) {
	v := &versionParameter{path: path, to: tag}

	resp, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(path),
	})
	if err != nil {
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return v, nil
		}
		return nil, fmt.Errorf("failed to read version parameter %q: %w", path, err)
	}

	v.from = aws.ToString(resp.Parameter.Value)
	v.existed = true
	return v, nil
}

// writeVersionParameter stores the deployed tag.
//
// Type is sent only when creating: SSM requires it on create and rejects a
// Type that disagrees with an existing parameter on overwrite, so omitting it
// preserves whatever type the parameter already has. Overwriting a value does
// not change the parameter's ARN, so a task definition referencing it keeps
// working and no secrets diff is produced.
func writeVersionParameter(ctx context.Context, client *ssm.Client, v *versionParameter) error {
	input := &ssm.PutParameterInput{
		Name:      aws.String(v.path),
		Value:     aws.String(v.to),
		Overwrite: aws.Bool(true),
	}
	if !v.existed {
		input.Type = ssmtypes.ParameterTypeString
	}

	if _, err := client.PutParameter(ctx, input); err != nil {
		return fmt.Errorf("failed to write version parameter %q: %w", v.path, err)
	}
	return nil
}

// describeVersionRollback returns the guidance to print when a deployment fails
// after the version parameter has already been written. The parameter then names
// a tag that is not running, which is exactly the disagreement it exists to
// prevent, so it must not be left silent.
func describeVersionRollback(v *versionParameter) string {
	if v == nil || !v.changed() {
		return ""
	}

	if !v.existed {
		return fmt.Sprintf(
			"WARNING: version parameter %s was created as %q but the deployment did not succeed.\n"+
				"         It now names a tag that is not running. Delete it or set it to the tag actually deployed:\n"+
				"           aws ssm delete-parameter --name %s\n",
			v.path, v.to, v.path,
		)
	}

	return fmt.Sprintf(
		"WARNING: version parameter %s was updated to %q but the deployment did not succeed.\n"+
			"         It now names a tag that is not running. Restore the previous value or re-run the deploy:\n"+
			"           aws ssm put-parameter --name %s --value %s --type String --overwrite\n",
		v.path, v.to, v.path, v.from,
	)
}
