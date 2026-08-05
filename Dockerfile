# One image per service, selected by the SERVICE build argument, so every
# binary is built from the same source tree with the same toolchain.
FROM golang:1.26-alpine AS build

ARG SERVICE
WORKDIR /src

# The module and build caches are mounted rather than baked in: a download cut
# short by the network is resumed by the next attempt instead of starting over,
# and four services building the same tree share one copy of the work.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/service ./cmd/${SERVICE}

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/service /usr/local/bin/service
COPY --from=build /src/migrations /migrations

ENTRYPOINT ["/usr/local/bin/service"]
