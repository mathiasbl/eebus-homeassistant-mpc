ARG BUILD_FROM=ghcr.io/home-assistant/base:latest
FROM $BUILD_FROM AS builder
RUN apk --no-cache add go
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /eebus-ha-bridge .

FROM $BUILD_FROM
WORKDIR /app
COPY --from=builder /eebus-ha-bridge .
COPY run.sh /
RUN chmod +x /run.sh
VOLUME ["/data"]
EXPOSE 4714
CMD [ "/run.sh" ]
