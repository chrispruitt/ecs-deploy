# ecs-deploy

A CLI tool for deploying container images and syncing SSM Parameter Store secrets to Amazon ECS services.

> Inspired by [justmiles/ecs-deploy](https://github.com/justmiles/ecs-deploy). This is a complete rewrite with a focus on secrets management, interactive deployment previews, and reliable rollout tracking.

---

## What it does

`ecs-deploy` wraps the three-step ECS deployment cycle — register a new task definition revision, update the service, and wait for the rollout — into a single command. It adds first-class support for keeping ECS container secrets in sync with SSM Parameter Store, so your environment variables stay consistent with what is stored in SSM without manual task definition edits.

Key capabilities:

- **Image tag update** — replace the tag on a named container's image without touching anything else in the task definition.
- **SSM secret sync** — discover all SSM parameters under one or more path prefixes and reconcile them against the container's `secrets` block. Additions, removals, and ARN updates are all handled.
- **Interactive diff** — before any AWS API write is made, the tool prints a deployment plan showing exactly what will change. You approve (or cancel) before anything is modified.
- **Rollout tracking** — polls `DescribeServices` every 10 seconds until the deployment reaches `COMPLETED` or fails, with a 30-minute timeout. ECS circuit breaker events are surfaced with actionable messages.
- **IAM role assumption** — pass `--role` to assume a cross-account or cross-service IAM role before any API calls.
- **CI/CD friendly** — `--auto-approve` skips the prompt; `--no-wait` exits as soon as the service is updated.

---

## Installation

```sh
go install github.com/chrispruitt/ecs-deploy@latest
```

Or download a pre-built binary from the [releases page](https://github.com/chrispruitt/ecs-deploy/releases).

---

## Usage

```
ecs-deploy --cluster <cluster> --service <service> --container <container> [flags]
```

### Flags

| Flag | Required | Description |
|---|---|---|
| `--cluster` | yes | ECS cluster name |
| `--service` | yes | ECS service name |
| `--container` | yes | Name of the container to update |
| `--image-tag` | one of image-tag or ssm-prefix | New image tag to deploy |
| `--secret-ssm-prefix` | one of image-tag or ssm-prefix | SSM path prefix to sync as container secrets (repeatable) |
| `--role` | no | IAM role ARN to assume before making API calls |
| `--auto-approve` | no | Skip the interactive approval prompt |
| `--no-wait` | no | Exit after registering the task definition without waiting for the rollout |

---

## Common use cases

### Deploy a new image tag

The most common use case: build a new image, push it to ECR, then call `ecs-deploy` with the new tag.

```sh
ecs-deploy \
  --cluster production \
  --service api \
  --container api \
  --image-tag v1.4.2
```

The tool prints the old and new image references for review, asks for confirmation, registers the new task definition, updates the service, and streams rollout progress until the deployment completes.

---

### Update environment variables from SSM

Use `--secret-ssm-prefix` to keep a container's `secrets` block in sync with a path in SSM Parameter Store. All parameters under the prefix are mapped to environment variable names by stripping the prefix from the parameter path.

For example, given these SSM parameters:

```
/myapp/prod/DB_HOST       → arn:aws:ssm:...:parameter/myapp/prod/DB_HOST
/myapp/prod/DB_PASSWORD   → arn:aws:ssm:...:parameter/myapp/prod/DB_PASSWORD
/myapp/prod/API_KEY       → arn:aws:ssm:...:parameter/myapp/prod/API_KEY
```

Running:

```sh
ecs-deploy \
  --cluster production \
  --service api \
  --container api \
  --secret-ssm-prefix /myapp/prod
```

…will inject `DB_HOST`, `DB_PASSWORD`, and `API_KEY` as ECS secrets (via `ValueFrom` ARN references). Parameters already present with the correct ARN are left untouched; parameters removed from SSM are dropped from the task definition; and new parameters are added — all shown in the diff before confirmation.

Secrets outside the managed prefix are never modified.

---

### Update secrets and image in one deploy

Both flags can be combined. The task definition is registered once with all changes applied.

```sh
ecs-deploy \
  --cluster production \
  --service api \
  --container api \
  --image-tag v1.4.2 \
  --secret-ssm-prefix /myapp/prod
```

---

### Multiple SSM prefixes

Pass `--secret-ssm-prefix` more than once to merge parameters from several paths. This is useful when secrets are split across shared and service-specific paths.

```sh
ecs-deploy \
  --cluster production \
  --service api \
  --container api \
  --secret-ssm-prefix /shared/prod \
  --secret-ssm-prefix /myapp/prod \
  --image-tag v1.4.2
```

---

### CI/CD pipeline (non-interactive)

Skip the prompt and exit immediately after the service is updated:

```sh
ecs-deploy \
  --cluster production \
  --service api \
  --container api \
  --image-tag "$IMAGE_TAG" \
  --secret-ssm-prefix /myapp/prod \
  --auto-approve \
  --no-wait
```

---

### Cross-account deploy with role assumption

```sh
ecs-deploy \
  --cluster production \
  --service api \
  --container api \
  --image-tag v1.4.2 \
  --role arn:aws:iam::123456789012:role/DeployRole
```

---

## How SSM secret syncing works

ECS supports injecting secrets into containers via `secrets` entries in the task definition, where each entry maps an environment variable name to an SSM Parameter ARN. `ecs-deploy` automates keeping this list current.

Given an SSM prefix like `/myapp/prod/`, the tool:

1. Fetches all parameters recursively under that path using `GetParametersByPath`.
2. Derives the environment variable name by stripping the prefix — `/myapp/prod/DB_HOST` becomes `DB_HOST`.
3. Diffs the result against the container's existing `secrets` block, considering only entries whose `ValueFrom` falls under the managed prefix.
4. Adds new parameters, removes deleted ones, and updates entries whose ARN has changed (e.g. after a parameter was deleted and recreated).
5. Leaves all secrets outside the prefix untouched.

The parameter values are never fetched or logged — only the ARNs are written to the task definition. ECS resolves the values at container startup using its own IAM permissions.

---

## AWS permissions required

The IAM principal (or assumed role) needs:

```json
{
  "Effect": "Allow",
  "Action": [
    "ecs:DescribeServices",
    "ecs:DescribeTaskDefinition",
    "ecs:RegisterTaskDefinition",
    "ecs:UpdateService",
    "ssm:GetParametersByPath"
  ],
  "Resource": "*"
}
```

If `--role` is used, also add `sts:AssumeRole` on the target role ARN.

---

## Building from source

```sh
git clone https://github.com/chrispruitt/ecs-deploy.git
cd ecs-deploy
go build -o ecs-deploy .
```

To cross-compile all platforms (requires [goreleaser](https://goreleaser.com/)):

```sh
make build-all
```
