# Build the binary. CGO is off so the result runs on a distroless static image.
FROM golang:1.27 AS build

WORKDIR /src

# Dependencies are copied first so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/kestrel ./cmd/kestrel

# Run as a non-root user with no shell and no package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/kestrel /kestrel

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/kestrel"]
