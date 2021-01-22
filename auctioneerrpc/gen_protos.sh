#!/bin/sh

set -e

# Generate the gRPC bindings for all proto files.
for file in ./*.proto; do
  protoc -I/usr/local/include -I. \
	  --go_out=plugins=grpc,paths=source_relative:. \
    "${file}"

done
