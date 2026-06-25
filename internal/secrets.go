package internal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

type secretChange struct {
	name string
	from string // old ValueFrom; empty when adding
	to   string // new ValueFrom; empty when removing
}

func (c secretChange) action() string {
	if c.from == "" {
		return "add"
	}
	if c.to == "" {
		return "remove"
	}
	return "update"
}

// fetchSSMParameters returns a map of envVarName -> parameterARN for all
// parameters under each prefix, using paginated GetParametersByPath calls.
func fetchSSMParameters(ctx context.Context, client *ssm.Client, prefixes []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, prefix := range prefixes {
		norm := normalizeSSMPrefix(prefix)
		var nextToken *string
		for {
			resp, err := client.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
				Path:           aws.String(norm),
				Recursive:      aws.Bool(true),
				WithDecryption: aws.Bool(false),
				NextToken:      nextToken,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to fetch SSM parameters under %q: %w", prefix, err)
			}
			for _, p := range resp.Parameters {
				name := paramEnvVarName(aws.ToString(p.Name), norm)
				result[name] = aws.ToString(p.ARN)
			}
			if resp.NextToken == nil {
				break
			}
			nextToken = resp.NextToken
		}
	}
	return result, nil
}

// computeSecretChanges diffs the named container's existing secrets against
// the fetched SSM parameters. Only secrets whose ValueFrom falls under one of
// the provided prefixes are considered "managed" — others are left untouched.
func computeSecretChanges(
	containers []ecstypes.ContainerDefinition,
	containerName string,
	prefixes []string,
	ssmParams map[string]string,
) ([]secretChange, error) {
	container := findContainerByName(containers, containerName)
	if container == nil {
		return nil, fmt.Errorf("container %q not found", containerName)
	}

	norms := normalizeSSMPrefixes(prefixes)

	// Index existing secrets that fall under the managed prefixes.
	existingManaged := make(map[string]string) // envVarName -> ValueFrom
	for _, s := range container.Secrets {
		if matchesAnySSMPrefix(aws.ToString(s.ValueFrom), norms) {
			existingManaged[aws.ToString(s.Name)] = aws.ToString(s.ValueFrom)
		}
	}

	var changes []secretChange

	// Removed: previously managed but absent from SSM.
	for name, oldFrom := range existingManaged {
		if _, exists := ssmParams[name]; !exists {
			changes = append(changes, secretChange{name: name, from: oldFrom})
		}
	}

	// Added or updated: present in SSM.
	for name, newARN := range ssmParams {
		if oldFrom, exists := existingManaged[name]; exists {
			if oldFrom != newARN {
				changes = append(changes, secretChange{name: name, from: oldFrom, to: newARN})
			}
			// Identical ValueFrom — no change entry.
		} else {
			changes = append(changes, secretChange{name: name, to: newARN})
		}
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].name < changes[j].name
	})

	return changes, nil
}

// applyChangesToContainers returns a new container slice with all prefix-managed
// secrets replaced by the fetched SSM parameters. Secrets outside the prefixes
// are preserved unchanged.
func applyChangesToContainers(
	containers []ecstypes.ContainerDefinition,
	containerName string,
	prefixes []string,
	ssmParams map[string]string,
) []ecstypes.ContainerDefinition {
	norms := normalizeSSMPrefixes(prefixes)
	result := make([]ecstypes.ContainerDefinition, len(containers))
	copy(result, containers)

	for i, c := range result {
		if aws.ToString(c.Name) != containerName {
			continue
		}
		// Keep secrets outside the managed prefixes.
		kept := make([]ecstypes.Secret, 0, len(c.Secrets)+len(ssmParams))
		for _, s := range c.Secrets {
			if !matchesAnySSMPrefix(aws.ToString(s.ValueFrom), norms) {
				kept = append(kept, s)
			}
		}
		// Add all current SSM parameters.
		for name, arn := range ssmParams {
			kept = append(kept, ecstypes.Secret{
				Name:      aws.String(name),
				ValueFrom: aws.String(arn),
			})
		}
		result[i].Secrets = kept
		break
	}

	return result
}

func confirmChanges(ctx context.Context, r io.Reader) (bool, error) {
	fmt.Print("Proceed with deployment? [y/N]: ")

	type scanResult struct {
		text string
		err  error
	}
	ch := make(chan scanResult, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		if scanner.Scan() {
			ch <- scanResult{text: scanner.Text()}
		} else {
			ch <- scanResult{err: scanner.Err()}
		}
	}()

	select {
	case <-ctx.Done():
		fmt.Println()
		return false, fmt.Errorf("deployment cancelled")
	case res := <-ch:
		if res.err != nil {
			return false, fmt.Errorf("failed to read input: %w", res.err)
		}
		ans := strings.TrimSpace(strings.ToLower(res.text))
		return ans == "y" || ans == "yes", nil
	}
}

func normalizeSSMPrefix(prefix string) string {
	if !strings.HasSuffix(prefix, "/") {
		return prefix + "/"
	}
	return prefix
}

func normalizeSSMPrefixes(prefixes []string) []string {
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = normalizeSSMPrefix(p)
	}
	return out
}

// paramEnvVarName strips the normalized prefix from a full SSM parameter path
// to produce the environment variable name (e.g. /app/prod/DB_HOST -> DB_HOST).
func paramEnvVarName(path, normalizedPrefix string) string {
	return strings.TrimPrefix(path, normalizedPrefix)
}

// matchesAnySSMPrefix reports whether a secret's ValueFrom (ARN or path)
// falls under one of the normalized prefixes.
func matchesAnySSMPrefix(valueFrom string, normalizedPrefixes []string) bool {
	path := valueFrom
	// Extract parameter path from an ARN: arn:aws:ssm:...:parameter/path/here
	if idx := strings.Index(valueFrom, ":parameter/"); idx != -1 {
		path = valueFrom[idx+len(":parameter"):]
	}
	for _, prefix := range normalizedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func findContainerByName(containers []ecstypes.ContainerDefinition, name string) *ecstypes.ContainerDefinition {
	for i := range containers {
		if aws.ToString(containers[i].Name) == name {
			return &containers[i]
		}
	}
	return nil
}
