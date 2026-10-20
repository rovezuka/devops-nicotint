FROM golang:1.25

WORKDIR /src

COPY go.mod ./

COPY *.go ./

RUN go build -o /usr/local/bin/api .

EXPOSE 8080

CMD ["api"]