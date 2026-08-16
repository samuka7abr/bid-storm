# go.mod declares go 1.25: the runtime dependencies are all pinned to their
# newest Go 1.23-compatible releases, but testcontainers-go — test-only, never
# in this binary — drags a docker/otel closure that no longer builds under 1.23.
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/auctiond ./cmd/auctiond

FROM alpine:3.20
RUN adduser -D -u 10001 auction
USER auction
COPY --from=build /out/auctiond /usr/local/bin/auctiond
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/auctiond"]
