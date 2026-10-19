# Changelog

## v0.4.0

### Added

- `--release-command "<cmd>"`: run a one-off task from the new task definition
  revision before the service is updated, and only continue if the named
  container exits 0. The ECS equivalent of Heroku's release phase / Fly's
  `release_command`, built for database migrations but generic. A failed
  release leaves the version parameter unwritten and the service untouched.
- `--release-timeout <duration>` (default `30m`, hours allowed): how long to
  wait for the release command before stopping the task and failing the deploy.
- `ecs-deploy run` subcommand: run a command as a one-off task on a service's
  infrastructure, at a given `--image-tag` (a new revision is registered) or at
  the current revision (`--no-register`), and exit with the container's exit
  code. Never updates the service or writes a version parameter.
- Log tailing for release and `run` tasks whose container logs through
  `awslogs`; a one-line hint for other drivers.
- Exit codes for the release step: `1` command failed, `2` task failed to
  start, `3` timed out.

### Changed

- The root deploy command gains two optional flags and is otherwise unchanged:
  without `--release-command` it makes the same AWS calls in the same order as
  v0.3.0. With it, the order becomes register revision -> release task ->
  version parameter -> `UpdateService` -> rollout.
- `DescribeServices` requests the service's tags only when a release task will
  be run (to decide `propagateTags`).
- A `--container` that does not exist in the task definition now fails before
  the plan is printed instead of after approval.

### IAM

- `--release-command` / `run` additionally need `ecs:RunTask`,
  `ecs:DescribeTasks`, `ecs:StopTask`, `logs:GetLogEvents`, and `iam:PassRole`
  on the task and execution roles. See the README.

## v0.3.0

- `--version-parameter` is rolled back when a deploy fails and the new tag is
  certain not to be running.

## v0.2.0

- `--version-parameter` records the deployed tag in SSM before the service is
  updated.

## v0.1.0

- Initial release: image tag update, SSM secret sync, interactive plan, rollout
  tracking, role assumption.
