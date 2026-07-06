# syntax=docker/dockerfile:1.5

FROM golang:1.25.6-alpine3.22 AS builder
RUN apk add --no-cache ca-certificates git openssh

# Build args map 1:1 to Jenkins environment variables (BUILD_NUMBER,
# GIT_BRANCH, GIT_COMMIT are set automatically by the Jenkins Git/Pipeline
# plugins; BUILD_DATE is computed in the Jenkinsfile). See Jenkinsfile.
ARG BUILD_NUMBER=local
ARG GIT_BRANCH="main"
ARG GIT_COMMIT="00000000"
ARG BUILD_DATE="today"
ARG SERVICE=synergy-geofence
ARG MODULE=github.com/sentinelgo/$SERVICE

ENV GO111MODULE=on \
    GOPRIVATE=github.com/sentinelgo/* \
    MODULE=$MODULE \
    BUILD_DATE=$BUILD_DATE \
    GIT_COMMIT=$GIT_COMMIT \
    GIT_BRANCH=$GIT_BRANCH \
    BUILD_NUMBER=$BUILD_NUMBER

RUN git config --global url.ssh://git@github.com/.insteadOf https://github.com/
RUN mkdir -m 700 /root/.ssh; \
    touch -m 600 /root/.ssh/known_hosts; \
    ssh-keyscan github.com > /root/.ssh/known_hosts

WORKDIR /src

COPY ../go.mod go.sum ./

RUN --mount=type=ssh \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY .. .

RUN --mount=type=ssh \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build \
    -ldflags="-s -w -X ${MODULE}/cmd.version=${BUILD_NUMBER} -X ${MODULE}/cmd.branch=${GIT_BRANCH} -X ${MODULE}/cmd.date=${BUILD_DATE}" -o service .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 geofence && \
    adduser -D -u 1000 -G geofence geofence

WORKDIR /app

COPY --from=builder /etc/ssl/certs /etc/ssl/certs
COPY --from=builder /src/service /app/service

USER geofence

EXPOSE 7010

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:7010/health || exit 1

ENTRYPOINT ["/app/service"]
CMD ["serve"]