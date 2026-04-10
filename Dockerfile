FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o clique-node ./cmd/clique-node


FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /build/clique-node /usr/local/bin/clique-node

EXPOSE 8550 9000 5052

ENTRYPOINT ["clique-node"]
CMD ["--config", "/app/config.toml"]
