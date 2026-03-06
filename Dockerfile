FROM golang:1.21-alpine

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN go build -o whatsmeow-unban-api

EXPOSE 8080

CMD ["./whatsmeow-unban-api"]
