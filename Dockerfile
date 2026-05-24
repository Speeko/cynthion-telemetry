# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go mod tidy && go build -o /out/cynthion-telemetry -trimpath -ldflags='-s -w' .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /data && chown app:app /data
COPY --from=builder /out/cynthion-telemetry /usr/local/bin/cynthion-telemetry
USER app
WORKDIR /
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/cynthion-telemetry"]
