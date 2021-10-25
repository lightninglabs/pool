FROM golang:1.17.1-bullseye

RUN go install github.com/golang/mock/mockgen@v1.6.0

WORKDIR /build

CMD ["/bin/bash", "-c", "go generate ./..."]
