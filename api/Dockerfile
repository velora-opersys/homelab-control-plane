FROM golang:1.23-alpine AS api-build
WORKDIR /src/api
COPY api/go.mod ./
RUN go mod download
COPY api/ .
RUN go mod tidy && go mod verify
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/axiom-api .

FROM golang:1.23-alpine AS agent-build
WORKDIR /src/agent
COPY agent/go.mod ./
COPY agent/ .
RUN mkdir -p /out && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/axiom-agent-linux-amd64 . && \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /out/axiom-agent-linux-arm64 .

FROM alpine:3.22
RUN adduser -D -H -u 10001 axiom && mkdir -p /opt/axiom/agents
COPY --from=api-build /out/axiom-api /usr/local/bin/axiom-api
COPY --from=agent-build /out/axiom-agent-linux-amd64 /opt/axiom/agents/axiom-agent-linux-amd64
COPY --from=agent-build /out/axiom-agent-linux-arm64 /opt/axiom/agents/axiom-agent-linux-arm64
RUN chmod 0555 /opt/axiom/agents/axiom-agent-linux-*
USER axiom
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/axiom-api"]
