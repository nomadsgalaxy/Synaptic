# Multi-stage build for Synaptic Core.
# Final image is ~15 MB based on `scratch` (no shell, just the binary).

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod ./
COPY go.sum* ./
COPY . .
# go mod tidy generates go.sum from go.mod if it's missing, so contributors
# don't have to commit it for a clean build to work.
RUN go mod tidy
# CGO_ENABLED=0 produces a fully static binary that runs in `scratch`.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=dev" \
    -o /out/sd-core .

FROM scratch
COPY --from=build /out/sd-core /sd-core
# Copy CA certs in case adapters need to talk out (they shouldn't but cheap insurance)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 9911
ENV SD_CORE_LISTEN=0.0.0.0:9911 \
    SD_DATA_DIR=/data
VOLUME ["/data"]
# Self-probe healthcheck — the binary's -healthcheck flag hits /healthz on
# the configured listen address and exits 0/1. Avoids shell + wget so the
# scratch base image still works.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/sd-core", "-healthcheck"]
ENTRYPOINT ["/sd-core"]
