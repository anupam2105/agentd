# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26.11-alpine AS build

WORKDIR /src

# Fetch modules separately so the layer caches when only source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Statically link so the distroless base image (no libc) can execute us.
ARG BUILD_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/agentd ./cmd/agentd \
 && mkdir -p /out/data

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/agentd /usr/local/bin/agentd
# The run/step journal lives here; mount a volume to keep it across restarts.
COPY --from=build --chown=65532:65532 /out/data /data
ENV AGENTD_DB_PATH=/data/agentd.db

USER nonroot:nonroot
EXPOSE 7700

ENTRYPOINT ["/usr/local/bin/agentd"]
