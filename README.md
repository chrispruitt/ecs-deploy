# ecs-deploy

A CLI tool for deploying container images and syncing SSM Parameter Store secrets to Amazon ECS services.

> Inspired by [justmiles/ecs-deploy](https://github.com/justmiles/ecs-deploy). This is a complete rewrite with a focus on secrets management, interactive deployment previews, and reliable rollout tracking.

---

## What it does

`ecs-deploy` wraps the three-step ECS deployment cycle — register a new task definition revision, update the service, and wait for the rollout — into a single command. It adds first-class support for keeping ECS container secrets in sync with SSM Parameter Store, so your environment variables stay consistent with what is stored in SSM without manual task definition edits.

Key capabilities:

- **Image tag update** — replace the tag on a named container's image without touching anything else in the task definition.
- **SSM secret sync** — discover all SSM parameters under one or more path prefixes and reconcile them against the container's `secrets` block. Additions, removals, and ARN updates are all handled.
- **Deployed-version recording** — `--version-parameter` writes the tag being deployed to an SSM parameter, so Terraform (or any IaC reading it) re-derives what is actually running instead of reverting the service on the next apply.
- **Interactive diff** — before any AWS API write is made, the tool prints a deployment plan showing exactly what will change. You approve (or cancel) before anything is modified.
- **Release command** — `--release-command` runs a one-off task from the new revision (database migrations, cache warm-ups, asset syncs) and only rolls the service if it exits 0 — the ECS equivalent of Heroku's release phase or Fly's `release_command`.
- **One-off tasks** — `ecs-deploy run` runs any command as a task on the service's infrastructure at a chosen image tag, without deploying, and exits with the container's exit code.
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
| `--version-parameter` | no | SSM parameter path to record `--image-tag` in, written before the deploy and rolled back if the deploy fails. Requires `--image-tag`. |
| `--release-command` | no | Command to run as a one-off task from the new revision before the service is updated; a non-zero exit aborts the deploy. See [Release commands](#release-commands). |
| `--release-timeout` | no | How long to wait for `--release-command` before stopping it and failing the deploy. Go duration, hours allowed (`45m`, `4h`). Default `30m`. |
| `--role` | no | IAM role ARN to assume before making API calls |
| `--auto-approve` | no | Skip the interactive approval prompt (covers the release step too) |
| `--no-wait` | no | Exit after registering the task definition without waiting for the rollout. The release command is still waited for — it gates the rollout. |

### `run` subcommand

```
ecs-deploy run --cluster <cluster> --service <service> --container <container> --command "<cmd>" [flags]
```

Runs a command as a one-off task on the service's infrastructure and exits with
the container's exit code. Never updates the service, never writes a version
parameter. See [One-off tasks](#one-off-tasks-ecs-deploy-run).

| Flag | Required | Description |
|---|---|---|
| `--cluster` | yes | ECS cluster name |
| `--service` | yes | ECS service whose task definition, network, roles and launch settings the task uses |
| `--container` | yes | Container to run the command in |
| `--command` | yes | Command to run, split shell-style (quotes respected) into the container command override |
| `--image-tag` | unless `--no-register` | Register a new revision with this tag on the container first, so the command runs at that version |
| `--no-register` | no | Run from the service's current task definition revision as-is |
| `--secret-ssm-prefix` | no | Sync secrets onto the new revision, as for a deploy (repeatable) |
| `--timeout` | no | How long to wait before stopping the task. Default `30m`; hours allowed (`4h`). |
| `--role` | no | IAM role ARN to assume |
| `--auto-approve` | no | Skip the approval prompt |

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

## Recording the deployed version

Infrastructure-as-code that manages the same ECS service needs to know which tag
is actually deployed, or its next apply reverts the service to whatever its
configuration hardcodes. The usual fix is an SSM parameter holding the current
tag, which the IaC reads instead of pinning a literal.

Keeping that parameter correct by hand is a step that fails silently when
forgotten: the deploy succeeds, the service runs the new tag, and nothing
complains until an apply quietly rolls it back. `--version-parameter` makes it
part of the deployment instead.

```sh
ecs-deploy \
  --cluster prod \
  --service my-service \
  --container my-service \
  --image-tag v1.4.2 \
  --secret-ssm-prefix /prod/my-service \
  --version-parameter /prod/my-service/VERSION
```

The plan shows the move before anything is written:

```
Deployment plan for container "my-service":
  image:  ...my-service:v1.4.1
          -> ...my-service:v1.4.2
  version: /prod/my-service/VERSION
           v1.4.1 -> v1.4.2
  secrets: no changes
```

Details worth knowing:

- **The write happens before the service is updated.** The parameter is often
  injected into the container too, and ECS resolves those values at task start —
  writing afterwards would leave the new task reporting the version it replaced.
  Writing first also means a failed write aborts before the service is touched.
- **A parameter that does not exist yet is created** as a `String`. When it does
  exist, its type is left alone.
- **Overwriting a value does not change the parameter ARN**, so a task definition
  that references it keeps working and no secrets diff is produced — including
  when the parameter sits under a path passed to `--secret-ssm-prefix`.
- **Redeploying the same tag writes nothing**, and the plan says
  `already <tag>` rather than showing a no-op change.
- **If the deployment fails after the write, the parameter is rolled back.** A
  parameter naming a tag that is not running is the exact disagreement it exists
  to prevent, so when the new tag is certain not to be running — registering the
  task definition or updating the service fails, or ECS reports the rollout
  `FAILED` (including a circuit-breaker rollback) — the previous value is put
  back. A parameter this deploy created is deleted instead.
- **An unknown outcome is not rolled back.** A timeout, an interrupt (Ctrl-C),
  or an API error while polling leaves the rollout running in ECS, and it may
  still complete; restoring the old tag then would create the disagreement
  rather than fix it. In those cases, and if the rollback itself fails, the tool
  prints the previous value and the command to restore it instead of exiting
  silently. `--no-wait` never sees the outcome, so it never rolls back.

---

## Release commands

Some changes need a step to run once, from the new image, before the new
version takes traffic — most often a database migration. `--release-command`
is that step, built into the deploy:

```sh
ecs-deploy \
  --cluster prod \
  --service api \
  --container api \
  --image-tag v1.5.0 \
  --version-parameter /prod/api/VERSION \
  --release-command "npm run db:migrate" \
  --release-timeout 1h
```

The plan shows it before anything is written:

```
Deployment plan for container "api":
  release: run `npm run db:migrate` in container "api" from the new revision (timeout 1h0m0s)
           the service is updated only if it exits 0
  image:  ...api:v1.4.2
          -> ...api:v1.5.0
  version: /prod/api/VERSION
           v1.4.2 -> v1.5.0
```

What happens, in order:

1. The new task definition revision is registered (image tag and secrets
   applied, exactly as without the flag).
2. A single task is started from that revision with `RunTask`, on the service's
   own launch type (or capacity provider strategy), platform version, subnets
   and security groups, with the command applied **only as a container
   override**. The registered revision is never modified to carry it, so
   Terraform (or whatever owns the task definition) stays the source of truth.
3. The named container's logs are tailed while it runs, when it logs through
   `awslogs`. For any other driver (`awsfirelens` etc.) one hint line is
   printed with the service and timestamp to search by.
4. The container is waited on — **the container, not the task**. Task
   definitions with essential sidecars (an OTEL collector, a FireLens log
   router) stay `RUNNING` after the application container exits, so the tool
   polls `DescribeTasks` until the named container is `STOPPED`, reads its exit
   code, then stops the task so the sidecars go too.
5. Exit code 0: the deploy continues exactly as it would without the flag —
   version parameter written, `UpdateService`, rollout tracked, same rollback
   semantics.
   Anything else: the deploy stops there. **The version parameter has not
   been written and the service has not been updated.** The only trace is the
   registered, unused revision.

Exit codes when the release step fails:

| Code | Meaning |
|---|---|
| `1` | The command ran and exited non-zero. The message includes the exit code, `stopCode`, `stoppedReason` and task ARN. |
| `2` | The task never ran the command: `RunTask` returned a failure (no capacity, bad role), or the container stopped without an exit code (image pull failure, missing secret, `TaskFailedToStart`). |
| `3` | `--release-timeout` elapsed. The task has been stopped. |

Details worth knowing:

- `--auto-approve` approves the release step along with the rest of the plan.
- `--no-wait` still waits for the release command — it gates the rollout — and
  only skips waiting for the rollout itself.
- Ctrl-C while the command is running stops the task before exiting, so an
  interrupted deploy does not leave a one-off task running unattended.
- The command string is split like a shell would (`"npm run migrate"` becomes
  `["npm", "run", "migrate"]`; quotes group arguments), but nothing is
  interpreted: no `&&`, pipes or variables. Wrap in `sh -c "..."` for those.
- The release task starts before the version parameter is written, so a
  parameter injected into the container still reads the previous value during
  the release step.
- The service's tags are copied onto the task when it has any. If that is
  denied, the task is started again without tags rather than failing the
  release.

---

## One-off tasks: `ecs-deploy run`

The same mechanism, without a deploy. `run` starts a command as a task on the
service's infrastructure and exits with the container's exit code:

```sh
ecs-deploy run \
  --cluster prod \
  --service api \
  --container api \
  --image-tag v1.5.0 \
  --command "npm run db:migrate" \
  --timeout 4h
```

By default it registers a new revision from the service's current one with
`--image-tag` applied (and `--secret-ssm-prefix`, if given), so the command
runs at the version you are about to deploy. Pass `--no-register` to run at
the version currently deployed instead, with no `--image-tag`:

```sh
ecs-deploy run --cluster prod --service api --container api --no-register \
  --command "node scripts/reindex.js --since 2026-01-01"
```

`run` never calls `UpdateService` and never writes a version parameter. It
prints the revision ARN it ran from and the task ARN, tails logs the same way
`--release-command` does, and uses the same exit codes (0, 1, 2, 3).

---

## Database migrations

The use case this was built for. Two patterns, depending on how long the
migration takes.

**Migrate as part of every deploy.** Add `--release-command` to the pipeline's
deploy step. A failing migration fails the deploy before the service changes;
a passing one is followed immediately by the rollout, so the schema and the
code that expects it land together.

```sh
ecs-deploy \
  --cluster prod --service api --container api \
  --image-tag "$IMAGE_TAG" \
  --version-parameter /prod/api/VERSION \
  --release-command "migrate" \
  --auto-approve
```

**Run a long or risky migration ahead of the deploy.** A backfill that takes
hours should not hold a deploy open. Run it first at the new tag, from a
terminal, with a timeout to match, then deploy with or without a release
command once it has finished:

```sh
ecs-deploy run \
  --cluster prod --service api --container api \
  --image-tag v1.5.0 \
  --command "migrate" \
  --timeout 6h
```

Both forms need the migration to be safe to run against the schema the
currently deployed version is using, since the old tasks keep serving traffic
until the rollout completes — the same expand/contract discipline any rolling
deploy requires.

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

`--version-parameter` additionally needs `ssm:GetParameter` and
`ssm:PutParameter` on that parameter.

`--release-command` and `ecs-deploy run` additionally need:

```json
{
  "Effect": "Allow",
  "Action": [
    "ecs:RunTask",
    "ecs:DescribeTasks",
    "ecs:StopTask",
    "logs:GetLogEvents"
  ],
  "Resource": "*"
},
{
  "Effect": "Allow",
  "Action": "iam:PassRole",
  "Resource": [
    "arn:aws:iam::123456789012:role/<task-role>",
    "arn:aws:iam::123456789012:role/<execution-role>"
  ]
}
```

`iam:PassRole` is needed on both the task role and the execution role of the
task definition, because `RunTask` passes them. `logs:GetLogEvents` is only
used to tail `awslogs` output; without it the tail prints a warning and the
task still runs to completion. Tag propagation (`ecs:TagResource`) is
best-effort: if it is denied the task is retried without it.

`ssm:DeleteParameter` is optional. It is only used to roll back a parameter that
a failed deploy created, since there is no previous value to restore. Without
it, a parameter that already exists still rolls back, and a newly created one
falls back to printing the `delete-parameter` command to run by hand.

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
