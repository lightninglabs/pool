#!/bin/bash

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
  # Generate the gRPC bindings for all proto files.
  for file in ./*.proto; do
    protoc -I/usr/local/include -I. -I.. \
      --go_out=plugins=grpc,paths=source_relative:. \
      "${file}"

    # Only generate REST stubs if requested.
    if [[ "$1" == "no-rest" ]]; then
      continue
    fi

    # Generate the REST reverse proxy.
    protoc -I/usr/local/include -I. -I.. \
      --grpc-gateway_out=logtostderr=true,paths=source_relative,grpc_api_configuration=rest-annotations.yaml:. \
      "${file}"

    # Finally, generate the swagger file which describes the REST API in detail.
    protoc -I/usr/local/include -I. -I.. \
      --swagger_out=logtostderr=true,grpc_api_configuration=rest-annotations.yaml:. \
      "${file}"
  done
}

# format formats the *.proto files with the clang-format utility.
function format() {
  find . -name "*.proto" -print0 | xargs -0 clang-format --style=file -i
}

# Compile and format the poolrpc package.
pushd poolrpc
format
generate
popd

pushd auctioneerrpc
format
generate no-rest
popd
