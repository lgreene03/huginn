FROM golang:1.25-alpine AS builder
WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o huginn ./cmd/huginn

FROM alpine:3.20
WORKDIR /app

COPY --from=builder /app/huginn .

EXPOSE 8081

CMD ["./huginn"]
