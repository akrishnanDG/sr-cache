# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/sr-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sr-proxy /sr-proxy
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/sr-proxy"]
