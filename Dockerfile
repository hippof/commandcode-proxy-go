# Build a static binary, then ship it alone in a scratch image.
FROM golang:1.23-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /commandcode-proxy ./cmd/commandcode-proxy

FROM scratch
# CA roots for the outbound HTTPS call to Command Code.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /commandcode-proxy /commandcode-proxy
# Bind to all interfaces inside the container; publish to loopback on the host.
ENV COMMANDCODE_PROXY_HOST=0.0.0.0 \
    COMMANDCODE_PROXY_PORT=8787
EXPOSE 8787
ENTRYPOINT ["/commandcode-proxy"]
