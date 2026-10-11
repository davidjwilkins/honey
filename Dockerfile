# Build a static binary
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /honey ./cmd/honey

# Run it in a minimal image, as a non-root user
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /honey /usr/local/bin/honey
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/honey"]
CMD ["-config", "/etc/honey/honey.toml"]
