# Both image references are immutable per-platform digests. Update them only
# together with a reviewed lock/attestation change.
FROM golang:1.25.0@sha256:f7414a0dc5a64713686cbc9f1e8a7379b66af63ef9ad15760b43db40e0b15d9c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/control-plane ./cmd/control-plane

FROM alpine:3.22@sha256:7c8cb692ae09657cbc4a3f3cbd0e8d5a2690ba38386aaaf252dbb060bf5eb2e6
RUN addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app
COPY --from=build --chown=10001:10001 /out/control-plane /control-plane
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/control-plane"]
