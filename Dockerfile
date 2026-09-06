FROM golang:1.24-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/storage-r2 ./cmd/server

FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates \
    && addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/storage-r2 ./storage-r2
USER app
EXPOSE 8002
ENTRYPOINT ["./storage-r2"]
