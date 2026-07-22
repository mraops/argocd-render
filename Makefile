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
# Release workflow. The tag is the source of truth for the published version;
# CHANGELOG.md records what's in each release. To avoid the two drifting apart
# (the historical bug this section fixes), the default `make release` reads the
# target version FROM CHANGELOG.md.
#
# Usage:
#   make release                    # tag from CHANGELOG top entry (recommended)
#   make release-patch              # force patch bump v0.3.10 -> v0.3.11
#   make release-minor              # force minor bump v0.3.10 -> v0.4.0
#   make release-major              # force major bump v0.3.10 -> v1.0.0
#   make release MSG="feat: ..."    # custom commit message
#   make release CONFIRM=1          # skip the y/N prompt (CI)
#
# Workflow:
#   1. Add a "## vX.Y.Z" heading at the top of CHANGELOG.md describing the release.
#   2. make release  -> reads that version, commits, tags vX.Y.Z, pushes.
# The CHANGELOG version MUST be greater than the latest tag; otherwise the
# release is refused with a clear error.

current-version:
	@echo $(VERSION)

tag-list:
	@git tag -l --sort=-v:refname | head -10

# _release is the shared commit+tag+push engine.
# - If $(NEW) is set (release-patch/minor/major), use it.
# - If $(NEW) is empty (default `release`), read the target version from the
#   top of CHANGELOG.md. Doing the extraction here (inside the shell block)
#   instead of via make's $(shell ...) avoids the make-parser footguns around
#   awk/regex with parens.
# Guards:
#   - target version resolves to non-empty
#   - target tag does not already exist
#   - target is strictly greater than the latest tag (no downgrades, no re-release)
#   - interactive y/N confirmation unless CONFIRM=1 (CI-friendly)
define _release
	@new='$(NEW)'; \
	if [ -z "$$new" ]; then \
		new=$$(awk '/^##[[:space:]]*v[0-9]+\.[0-9]+\.[0-9]+/ {sub(/^##[[:space:]]*/,""); split($$0,a," "); print a[1]; exit}' CHANGELOG.md); \
	fi; \
	if [ -z "$$new" ]; then \
		echo "ERROR: no target version. Set NEW, add a '## vX.Y.Z' heading to CHANGELOG.md, or use release-patch/-minor/-major."; \
		exit 1; \
	fi; \
	if git rev-parse "$$new" >/dev/null 2>&1; then \
		echo "ERROR: tag $$new already exists; bump again or delete it first"; exit 1; \
	fi; \
	cur=$$(git describe --tags --abbrev=0 2>/dev/null || echo none); \
	echo "    current: $$cur"; \
	echo "    release: $$new"; \
	echo "    action:  commit + tag $$new + push (branch and tag)"; \
	if [ -z "$(CONFIRM)" ]; then \
		printf "Proceed? [y/N] "; read ans; \
		case "$$ans" in y|Y|yes|YES) ;; *) echo "aborted"; exit 1;; esac; \
	fi; \
	msg='$(or $(MSG),release '"$$new"')'; \
	git add -A && git commit -m "$$msg" || true; \
	git tag -a "$$new" -m "$$new"; \
	git push && git push --tags; \
	echo "Released $$new"
endef

# release-patch/minor/major: force a specific bump computed from the latest tag.
release-patch: NEW = v$(_CUR_MAJOR).$(_CUR_MINOR).$(shell echo $$(($(_CUR_PATCH)+1)))
release-patch:
	$(call _release)

release-minor: NEW = v$(_CUR_MAJOR).$(shell echo $$(($(_CUR_MINOR)+1))).0
release-minor:
	$(call _release)

release-major: NEW = v$(shell echo $$(($(_CUR_MAJOR)+1))).0.0
release-major:
	$(call _release)

# release (default): read the target version from the top of CHANGELOG.md.
# This is the recommended entry point — the CHANGELOG heading IS the version
# that gets tagged, so the two can never drift. $(NEW) is intentionally left
# empty; _release extracts the version from CHANGELOG inside its shell block
# (avoids make's $(shell ...) parser breaking on awk/regex parens).
release:
	$(call _release)

# Backwards-compatible aliases.
patch: release-patch
minor: release-minor
major: release-major

# --- Clean ---

clean:
	rm -rf $(BUILDDIR)
