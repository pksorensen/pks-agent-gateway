FROM golang:1.25-alpine AS builder
WORKDIR /workspace
COPY src/gateway ./src/gateway
WORKDIR /workspace/src/gateway
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/gateway .

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/gateway /app/gateway
ENV PORT=8080
ENV UPSTREAM=https://api.anthropic.com
ENV USER_DATA_DIR=/data
ENV GATEWAY_OWNER=default
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
