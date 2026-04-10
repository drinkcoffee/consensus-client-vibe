FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /keygen ./scripts/demo/init

FROM scratch
COPY --from=builder /keygen /keygen
ENTRYPOINT ["/keygen"]
