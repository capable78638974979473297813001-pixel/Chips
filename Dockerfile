# syntax=docker/dockerfile:1
# Build on the builder's own platform and cross-compile, so multi-arch
# images (docker buildx --platform linux/amd64,linux/arm64) build fast.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY examples/siliconrouter.yaml /etc/siliconrouter/siliconrouter.yaml
ENV SILICONROUTER_CONFIG=/etc/siliconrouter/siliconrouter.yaml
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/siliconrouter"]
