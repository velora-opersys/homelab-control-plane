FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# Resolve and verify the complete module graph before compiling. This also
# makes Docker build failures surface as dependency errors instead of an
# opaque compile-step failure.
RUN go mod tidy && go mod verify
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/axiom-api .

FROM alpine:3.22
RUN adduser -D -H -u 10001 axiom
USER axiom
COPY --from=build /out/axiom-api /usr/local/bin/axiom-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/axiom-api"]
