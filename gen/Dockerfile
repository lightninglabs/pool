FROM golang:1.19.4-bullseye

RUN go install github.com/golang/mock/mockgen@73266f9366fcf2ccef0b880618e5a9266e4136f4

WORKDIR /build

CMD ["/bin/bash", "-c", "go generate ./gen.go"]
