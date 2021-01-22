#!/bin/sh

set -e

# Generate the gRPC bindings for all proto files.
for file in ./*.proto
do
	protoc -I/usr/local/include -I. \
	  --go_out=plugins=grpc,paths=source_relative:. \
		"${file}"

  # Generate the REST reverse proxy.
  protoc -I/usr/local/include -I. \
    --grpc-gateway_out=logtostderr=true,paths=source_relative,grpc_api_configuration=rest-annotations.yaml:. \
    "${file}"
  
  # Finally, generate the swagger file which describes the REST API in detail.
  protoc -I/usr/local/include -I. \
    --swagger_out=logtostderr=true,grpc_api_configuration=rest-annotations.yaml:. \
    "${file}"

done
