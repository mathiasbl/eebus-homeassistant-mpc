FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /eebus-ha-bridge .

FROM alpine:3.20
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /eebus-ha-bridge .
VOLUME ["/data"]
EXPOSE 4714
ENTRYPOINT ["/app/eebus-ha-bridge"]
