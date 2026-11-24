FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /llm-gateway .

FROM alpine:3.23
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /llm-gateway /usr/local/bin/llm-gateway
RUN mkdir /data && chown 65532:65532 /data
WORKDIR /data
COPY gateway.example.toml /etc/llm-gateway.toml
USER 65532:65532
ENV LISTEN_ADDR=:8080 CONFIG_FILE=/etc/llm-gateway.toml
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["llm-gateway"]
