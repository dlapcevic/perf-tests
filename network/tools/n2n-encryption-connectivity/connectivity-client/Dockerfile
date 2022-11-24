# Build image
FROM golang:1.19 AS build-env

ARG gopkg=k8s.io/perf-tests/network/tools/connectivity-client
ADD [".", "/go/src/$gopkg"]
WORKDIR /go/src/$gopkg

RUN go build -o /connectivity-client

# Runtime image
FROM alpine:3.16
# Need to install curl to send requests to pods
RUN apk --no-cache add curl

COPY --from=build-env /connectivity-client /connectivity-client

# Make sure that when compiled on debian, it can run in alpine/musl
RUN mkdir /lib64 && ln -s /lib/libc.musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2

ENTRYPOINT ["/connectivity-client"]
