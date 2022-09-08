#!/bin/bash

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
  # Generate the gRPC bindings for all proto files.
  for file in ./*.proto; do
    protoc -I/usr/local/include -I. -I.. \
      --go_out . --go_opt paths=source_relative \
      --go-grpc_out . --go-grpc_opt paths=source_relative \
      "${file}"

    # Only generate REST stubs if requested.
    if [[ "$1" == "no-rest" ]]; then
      continue
    fi

    # Generate the REST reverse proxy.
    annotationsFile=${file//proto/yaml}
    protoc -I/usr/local/include -I. -I.. \
      --grpc-gateway_out . \
      --grpc-gateway_opt logtostderr=true \
      --grpc-gateway_opt paths=source_relative \
      --grpc-gateway_opt grpc_api_configuration=${annotationsFile} \
      "${file}"

    # Finally, generate the swagger file which describes the REST API in detail.
    protoc -I/usr/local/include -I. -I.. \
      --openapiv2_out . \
      --openapiv2_opt logtostderr=true \
      --openapiv2_opt grpc_api_configuration=${annotationsFile} \
      --openapiv2_opt json_names_for_fields=false \
      "${file}"

    # Generate the JSON/WASM client stubs.
    falafel=$(which falafel)
    pkg="poolrpc"
    manual_import="github.com/lightninglabs/pool/auctioneerrpc"
    opts="package_name=$pkg,manual_import=$manual_import,api_prefix=1,js_stubs=1"
    protoc -I/usr/local/include -I. -I.. \
           --plugin=protoc-gen-custom=$falafel\
           --custom_out=. \
           --custom_opt="$opts" \
           trader.proto

    # We've got a bit of a weird setup with the same RPC package being split over
    # two golang packages. We can't really fix this without breaking a lot so it's
    # easier to fix stuff explicitly here.
    sed -i 's/BatchSnapshotRequest/auctioneerrpc.BatchSnapshotRequest/g' trader.pb.json.go
    sed -i 's/BatchSnapshotsRequest/auctioneerrpc.BatchSnapshotsRequest/g' trader.pb.json.go

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
