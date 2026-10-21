FROM golang:1.26.8-bookworm

RUN go install go.uber.org/mock/mockgen@v0.4.0

WORKDIR /build

CMD ["/bin/bash", "-c", "go generate ./gen.go"]
