# syntax=docker/dockerfile:1

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY mm-bot .

# Disable file logging by default in container (stdout captured by K8s)
# Override with LOG_DIR=/data/logs + volume mount if needed
ENV LOG_DIR=none

EXPOSE 8080
CMD ["./mm-bot"]