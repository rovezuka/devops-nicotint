# Один Dockerfile на оба сервиса:
#   docker build --build-arg APP=api    -t shop-api:1.0.0 .
#   docker build --build-arg APP=worker -t shop-worker:1.0.0 .
FROM golang:1.26-alpine AS build
ARG APP=api
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${APP}

# distroless/static: без шелла и пакетного менеджера, пользователь nonroot (65532)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app"]
