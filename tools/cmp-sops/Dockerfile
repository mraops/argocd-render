# Build helm-repo-sync: a tiny Go CLI that materializes helm's repositories.yaml
# from ArgoCD repository Secrets, so private helm deps resolve inside the CMP
# sidecar. Pure stdlib, no external Go deps.
FROM golang:1.24-alpine AS build-helm-repo-sync
WORKDIR /src
COPY helm-repo-sync/go.mod helm-repo-sync/main.go ./
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /helm-repo-sync .

FROM ubuntu:25.10 AS download
WORKDIR /custom-tools/
COPY  sops-generate.sh .
COPY  --from=build-helm-repo-sync /helm-repo-sync /custom-tools/helm-repo-sync
RUN   apt-get update && apt-get install -y --no-install-recommends curl ca-certificates git && rm -rf /var/lib/apt/lists/*
RUN   curl -fSL --retry 5 --retry-delay 3 -o /custom-tools/sops https://github.com/getsops/sops/releases/download/v3.13.2/sops-v3.13.2.linux.amd64
RUN   curl -fSL --retry 5 --retry-delay 3 -o /custom-tools/yq https://github.com/mikefarah/yq/releases/download/v4.53.3/yq_linux_amd64
RUN   curl -fSL --retry 5 --retry-delay 3 https://get.helm.sh/helm-v3.19.2-linux-amd64.tar.gz | tar -xz -C /custom-tools --strip-components=1 linux-amd64/helm
RUN   chmod +x /custom-tools/sops /custom-tools/yq /custom-tools/helm /custom-tools/sops-generate.sh /custom-tools/helm-repo-sync

# helm-secrets plugin: install into a fixed, uid-999-readable location.
# Cache/config live in /tmp (mounted in the sidecar, writable under uid 999).
# ENV is set here for `helm plugin install` AND re-declared in the final stage
# (multi-stage does not carry ENV across stages).
ENV HELM_PLUGINS=/opt/helm-plugins \
  HELM_CACHE_HOME=/tmp/helm-cache \
  HELM_CONFIG_HOME=/tmp/helm-config \
  XDG_CACHE_HOME=/tmp/helm-cache \
  XDG_CONFIG_HOME=/tmp/helm-config
RUN   /custom-tools/helm plugin install https://github.com/jkroepke/helm-secrets.git && \
  chmod -R a+rX /opt/helm-plugins

FROM quay.io/argoproj/argocd:v3.4.4
COPY --from=download /custom-tools/ /usr/local/bin/
COPY --from=download /opt/helm-plugins /opt/helm-plugins
# Re-declare ALL helm env vars in the final stage (ENV does not carry across
# multi-stage builds). Without HELM_CONFIG_HOME helm looks for repositories.yaml
# under the default ~/.config/helm and never finds the one helm-repo-sync writes,
# so private helm dependencies would fail to resolve.
ENV HELM_PLUGINS=/opt/helm-plugins \
  HELM_CACHE_HOME=/tmp/helm-cache \
  HELM_CONFIG_HOME=/tmp/helm-config \
  XDG_CACHE_HOME=/tmp/helm-cache \
  XDG_CONFIG_HOME=/tmp/helm-config
