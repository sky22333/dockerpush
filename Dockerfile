FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download && go mod verify

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags='-w -s -extldflags "-static"' \
    -a -installsuffix cgo \
    -o dockerpush main.go

FROM alpine:latest

RUN apk add --no-cache ca-certificates && \
    rm -rf /var/cache/apk/*

COPY --from=builder /app/dockerpush /dockerpush

EXPOSE 5002

ENTRYPOINT ["/dockerpush"]