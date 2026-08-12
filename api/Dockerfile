FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hcp-api .

FROM alpine:3.22
RUN adduser -D -H -u 10001 hcp
USER hcp
COPY --from=build /out/hcp-api /usr/local/bin/hcp-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hcp-api"]
