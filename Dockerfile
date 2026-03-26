# syntax=docker/dockerfile:1

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY mm-bot .

EXPOSE 8080
CMD ["./mm-bot"]