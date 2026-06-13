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
        patch minor major release \
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
# Usage: make release MSG="feat: new feature"

current-version:
	@echo $(VERSION)

tag-list:
	@git tag -l --sort=-v:refname | head -10

patch:
	$(eval NEW := v$(_CUR_MAJOR).$(_CUR_MINOR).$(shell echo $$(($(_CUR_PATCH)+1))))
	$(eval MSG := $(or $(MSG),release $(NEW)))
	@git add -A && git commit -m "$(MSG)" || true
	@git tag $(NEW)
	@git push && git push --tags
	@echo "Released $(NEW)"

minor:
	$(eval NEW := v$(_CUR_MAJOR).$(shell echo $$(($(_CUR_MINOR)+1))).0)
	$(eval MSG := $(or $(MSG),release $(NEW)))
	@git add -A && git commit -m "$(MSG)" || true
	@git tag $(NEW)
	@git push && git push --tags
	@echo "Released $(NEW)"

major:
	$(eval NEW := v$(shell echo $$(($(_CUR_MAJOR)+1))).0.0)
	$(eval MSG := $(or $(MSG),release $(NEW)))
	@git add -A && git commit -m "$(MSG)" || true
	@git tag $(NEW)
	@git push && git push --tags
	@echo "Released $(NEW)"

release: patch
	@:

# --- Clean ---

clean:
	rm -rf $(BUILDDIR)
