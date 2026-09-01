FROM golang:1.21-alpine AS build
WORKDIR /app
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o server .

FROM alpine:3.19
WORKDIR /app
COPY --from=build /app/server .
EXPOSE 8080
CMD ["./server"]
