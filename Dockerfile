# Build stage
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/dread-server ./cmd/server

# Run stage
FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=build /bin/dread-server /bin/dread-server
EXPOSE 8080
ENTRYPOINT ["/bin/dread-server"]
