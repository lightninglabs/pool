FROM golang:1.19.4

RUN apt-get update && apt-get install -y git
ENV GOCACHE=/tmp/build/.cache
ENV GOMODCACHE=/tmp/build/.modcache

COPY . /tmp/tools

RUN cd /tmp \
  && mkdir -p /tmp/build/.cache \
  && mkdir -p /tmp/build/.modcache \
  && cd /tmp/tools \
  && go install -trimpath -tags=tools github.com/golangci/golangci-lint/cmd/golangci-lint \
  && chmod -R 777 /tmp/build/ \
  && git config --global --add safe.directory /build 
  # The last line is needed to ensure that go build is able to gather 
  # information from the vsc used in the builds to get the commit hash. 

WORKDIR /build
