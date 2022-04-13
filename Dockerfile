FROM --platform=${BUILDPLATFORM} golang:1.16-alpine as builder

# Copy in the local repository to build from.
COPY . /go/src/github.com/lightninglabs/pool

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

# Explicitly turn on the use of modules (until this becomes the default).
ENV GO111MODULE on

# Install dependencies and install/build pool.
RUN apk add --no-cache --update alpine-sdk \
    git \
    make \
&&  cd /go/src/github.com/lightninglabs/pool \
&&  make install

# Start a new, final image to reduce size.
FROM --platform=${BUILDPLATFORM} alpine as final

# Expose poold ports (gRPC and REST).
EXPOSE 12010 8281

# Copy the binaries from the builder image.
COPY --from=builder /go/bin/pool /bin/
COPY --from=builder /go/bin/poold /bin/

# Add bash.
RUN apk add --no-cache \
    bash \
    ca-certificates

ENTRYPOINT ["poold"]
