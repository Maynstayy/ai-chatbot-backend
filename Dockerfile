# Build
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dbtool ./cmd/dbtool

# Run: no shell, non-root. Configuration comes from the environment (never
# bake .env into the image).
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/api /out/dbtool /app/
COPY testdata/catalog.json /app/testdata/catalog.json
COPY migrations /app/migrations
ENV APP_ENV=production APP_PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/api"]
