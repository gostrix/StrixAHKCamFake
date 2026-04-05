FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /strixahkcamfake .

FROM alpine:3.21

RUN apk add --no-cache ffmpeg

COPY --from=builder /strixahkcamfake /usr/local/bin/strixahkcamfake

WORKDIR /data
EXPOSE 51826/tcp 8443/udp

ENTRYPOINT ["strixahkcamfake"]
