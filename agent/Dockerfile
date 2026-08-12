FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/homelab-agent .
FROM scratch
COPY --from=build /out/homelab-agent /homelab-agent
ENTRYPOINT ["/homelab-agent"]
