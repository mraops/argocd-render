FROM golang:1.24-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
COPY templates/ templates/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X main.appVersion=${VERSION}" \
    -o /argocd-render .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates git curl && \
    curl -fsSL -o /usr/local/bin/sops \
      https://github.com/getsops/sops/releases/download/v3.12.2/sops-v3.12.2.linux.amd64 && \
    chmod +x /usr/local/bin/sops && \
    curl -fsSL https://get.helm.sh/helm-v3.19.2-linux-amd64.tar.gz \
      | tar -xz -C /usr/local/bin --strip-components=1 linux-amd64/helm
COPY --from=builder /argocd-render /usr/local/bin/argocd-render
ENTRYPOINT ["argocd-render"]
