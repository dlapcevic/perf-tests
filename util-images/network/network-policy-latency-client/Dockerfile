# Build image
FROM golang:1.18 AS build-env

# Need to install curl to send requests to pods
#RUN apk update && apk add curl

ARG gopkg=k8s.io/perf-tests/util-images/network/network-policy-latency-client
ADD [".", "/go/src/$gopkg"]
WORKDIR /go/src/$gopkg

RUN go build -o /network-policy-latency-client

# Runtime image
FROM alpine:3.15
# Need to install curl to send requests to pods
RUN apk --no-cache add curl
#RUN apk update && apk add curl

COPY --from=build-env /network-policy-latency-client /network-policy-latency-client

# Make sure that when compiled on debian, it can run in alpine/musl
RUN mkdir /lib64 && ln -s /lib/libc.musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2

ENTRYPOINT ["/network-policy-latency-client"]
