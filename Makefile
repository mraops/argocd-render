BINARY  = argocd-render
IMAGE   ?= argocd-render
TAG     ?= latest
VERSION = $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -ldflags "-s -w -X main.appVersion=$(VERSION)"
GO      = go
BUILDDIR= build

# Parse current semver from latest tag
_CUR_MAJOR = $(shell git tag -l --sort=-v:refname | head -1 | sed -n 's/v\([0-9]*\)\..*/\1/p')
_CUR_MINOR = $(shell git tag -l --sort=-v:refname | head -1 | sed -n 's/v[0-9]*\.\([0-9]*\)\..*/\1/p')
_CUR_PATCH = $(shell git tag -l --sort=-v:refname | head -1 | sed -n 's/v[0-9]*\.[0-9]*\.\([0-9]*\)/\1/p')

.PHONY: build build-linux-amd64 build-linux-arm64 build-darwin-arm64 build-all \
        image image-arm64 image-all push push-arm64 push-all \
        patch minor major release release-patch release-minor release-major \
        tag-list current-version clean tidy

# --- Build ---

build:
	@mkdir -p $(BUILDDIR)
	$(GO) build $(LDFLAGS) -o $(BUILDDIR)/$(BINARY) .

build-linux-amd64:
	@mkdir -p $(BUILDDIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o $(BUILDDIR)/$(BINARY)-linux-amd64 .

build-linux-arm64:
	@mkdir -p $(BUILDDIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o $(BUILDDIR)/$(BINARY)-linux-arm64 .

build-darwin-arm64:
	@mkdir -p $(BUILDDIR)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build $(LDFLAGS) -o $(BUILDDIR)/$(BINARY)-darwin-arm64 .

build-all: build-linux-amd64 build-linux-arm64 build-darwin-arm64

tidy:
	$(GO) mod tidy

# --- Docker ---

image:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

image-arm64:
	docker build --platform linux/arm64 --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG)-arm64 .

image-all: image image-arm64

push:
	docker push $(IMAGE):$(TAG)

push-arm64:
	docker push $(IMAGE):$(TAG)-arm64

push-all: push push-arm64

# --- Git versioning ---
# Bump the version, commit (with CHANGELOG + any staged work), tag and push.
# Usage:
#   make release             # patch bump (v0.3.10 -> v0.3.11)
#   make release-patch       # same as release
#   make release-minor       # minor bump (v0.3.10 -> v0.4.0)
#   make release-major       # major bump (v0.3.10 -> v1.0.0)
#   make release MSG="feat: ..."   # custom commit message
#
# Set CHANGELOG.md heading to the version the bump will produce BEFORE running
# release, so the committed CHANGELOG and the tag match.

current-version:
	@echo $(VERSION)

tag-list:
	@git tag -l --sort=-v:refname | head -10

# _release is the shared commit+tag+push engine. Callers set $(NEW) first.
# It refuses to run when the next tag already exists (e.g. CHANGELOG written
# ahead but tag created manually) to avoid a confusing double-release.
define _release
	@new='$(NEW)'; \
	if [ -z "$$new" ]; then echo "ERROR: NEW is empty (no previous tag to bump?)"; exit 1; fi; \
	if git rev-parse "$$new" >/dev/null 2>&1; then \
		echo "ERROR: tag $$new already exists; bump again or delete it first"; exit 1; \
	fi; \
	msg='$(or $(MSG),release '"$$new"')'; \
	git add -A && git commit -m "$$msg" || true; \
	git tag "$$new"; \
	git push && git push --tags; \
	echo "Released $$new"
endef

release-patch: NEW = v$(_CUR_MAJOR).$(_CUR_MINOR).$(shell echo $$(($(_CUR_PATCH)+1)))
release-patch:
	$(call _release)

release-minor: NEW = v$(_CUR_MAJOR).$(shell echo $$(($(_CUR_MINOR)+1))).0
release-minor:
	$(call _release)

release-major: NEW = v$(shell echo $$(($(_CUR_MAJOR)+1))).0.0
release-major:
	$(call _release)

# Backwards-compatible aliases.
patch: release-patch
minor: release-minor
major: release-major

# Default release = patch bump.
release: release-patch
	@:

# --- Clean ---

clean:
	rm -rf $(BUILDDIR)
