# Build a static binary
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /honey ./cmd/honey
# A directory for the persistent cache (cache.persist), which the
# non-root user can write to; mount a volume here to keep it
RUN mkdir -p /out/var/lib/honey

# Run it in a minimal image, as a non-root user
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /honey /usr/local/bin/honey
COPY --from=build --chown=65532:65532 /out/var/lib/honey /var/lib/honey
# Plain HTTP; and with [tls], HTTPS and HTTP/3 (e.g. listen = ":80" and
# tls.listen = ":443": publish 80, 443 and 443/udp)
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/honey"]
CMD ["-config", "/etc/honey/honey.toml"]
