#!/bin/bash

set -e

# Directory of the script file, independent of where it's called from.
DIR="$(cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd)"

echo "Building mockgen docker image..."
docker build -t pool-gen-builder .

echo "Generating and formatting mock files..."
docker run \
  --rm \
  -v "$DIR/../:/build" \
  pool-gen-builder
