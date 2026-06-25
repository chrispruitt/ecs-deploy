IMAGE        := ecs-deploy
AWS_PROFILE  ?= core-ops.Admin
S3_BUCKET    := 889738907650-devops-artifacts
S3_PREFIX    := ecs-deploy

# Resolved from the tag on HEAD; empty if HEAD is not on a tag.
VERSION := $(shell git describe --tags --exact-match 2>/dev/null)

.PHONY: build run version build-all release clean

build:
	docker build -t $(IMAGE) .

run: build
	docker run --rm -it \
		-v ~/.aws:/root/.aws:ro \
		-e AWS_PROFILE=$(AWS_PROFILE) \
		-e AWS_REGION=$(AWS_REGION) \
		$(IMAGE) --help

version:
	@if [ -z "$(VERSION)" ]; then \
		echo "HEAD is not on a tag; tag it with 'git tag vX.Y.Z' before running 'make release'."; \
		exit 1; \
	fi
	@echo $(VERSION)

# Local dry-run: cross-compiles all platforms into dist/ without uploading.
build-all:
	goreleaser release --snapshot --clean --skip=publish

# Tag-driven release: builds, uploads to s3://.../ecs-deploy/<tag>/, then
# mirrors the same artifacts to s3://.../ecs-deploy/latest/.
release: version
	@aws sts get-caller-identity --profile $(AWS_PROFILE) > /dev/null
	AWS_PROFILE=$(AWS_PROFILE) goreleaser release --clean
	aws --profile $(AWS_PROFILE) s3 sync \
		s3://$(S3_BUCKET)/$(S3_PREFIX)/$(VERSION)/ \
		s3://$(S3_BUCKET)/$(S3_PREFIX)/latest/ \
		--delete
	@echo $(VERSION) | aws --profile $(AWS_PROFILE) s3 cp - \
		s3://$(S3_BUCKET)/$(S3_PREFIX)/latest/VERSION

clean:
	rm -rf dist
