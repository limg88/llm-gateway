FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /llm-gateway .

# The CLIs are Node programs; the gateway only starts them.
FROM node:22-slim
RUN npm install -g @anthropic-ai/claude-code @openai/codex && npm cache clean --force
COPY --from=build /llm-gateway /usr/local/bin/llm-gateway
USER node
ENV LISTEN_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD node -e "fetch('http://127.0.0.1:8080/health').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"
ENTRYPOINT ["llm-gateway"]
