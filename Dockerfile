# One image, both binaries. The server is the default entrypoint; run the
# agent with --entrypoint /usr/local/bin/voidgrid-backup-agent.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Tests gate the image: a failing test fails the build.
RUN go vet ./... && go test ./...
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/voidgrid/voidgrid-backup/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
# /data must be writable by whichever uid runs the container -- the
# examples set this explicitly (`user:` in docker-compose.yaml) rather than
# relying on this base image's own default, so any uid works, root
# included (the agent needs root, for the Docker/libvirt sockets).
VOLUME /data
EXPOSE 8080 9443
ENTRYPOINT ["/usr/local/bin/voidgrid-backup-server"]
