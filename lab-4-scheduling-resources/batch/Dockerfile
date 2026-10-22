FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /batch .

FROM scratch
COPY --from=build /batch /batch
USER 65534:65534
ENTRYPOINT ["/batch"]
