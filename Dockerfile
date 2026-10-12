# syntax=docker/dockerfile:1
# Builds one Go service from the repository root:
#   docker build --build-arg SERVICE=worker .
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot

FROM golang:1.27 AS build
ARG SERVICE
RUN test -n "$SERVICE" || (echo "build arg SERVICE is required" && false)
ENV CGO_ENABLED=0 GOWORK=off
WORKDIR /src
COPY shared/go.mod shared/go.sum shared/
COPY ${SERVICE}/go.mod ${SERVICE}/go.sum ${SERVICE}/
RUN --mount=type=cache,target=/go/pkg/mod go -C ${SERVICE} mod download
COPY shared/ shared/
COPY ${SERVICE}/ ${SERVICE}/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go -C ${SERVICE} build -trimpath -ldflags="-s -w" -o /out/service .

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/service /usr/local/bin/service
ENTRYPOINT ["/usr/local/bin/service"]
