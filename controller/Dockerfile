FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o /app/main .

FROM scratch

WORKDIR /app

COPY --from=builder /app/main .

ENTRYPOINT ["./main"]