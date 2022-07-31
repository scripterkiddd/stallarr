FROM golang as builder
ENV CGO_ENABLED=0
WORKDIR /go/src/github.com/scripterkiddd/stallarr
RUN apt-get update && apt-get install -y xz-utils
ADD https://github.com/upx/upx/releases/download/v3.96/upx-3.96-amd64_linux.tar.xz /tmp/
RUN mkdir -p /tmp/upx && tar -xJf /tmp/upx-3.96-amd64_linux.tar.xz -C /tmp/upx --strip-components=1 \
  && cp -rf /tmp/upx/* /usr/bin/ \
  && rm -rf /tmp/*
ADD . .
RUN go get ./...
RUN go build -tags 'netgo osusergo' -ldflags '-extldflags "-static"' -o /tmp/stallarr
RUN upx --brute /tmp/stallarr

FROM scratch

COPY --from=builder /tmp/stallarr /go/bin/stallarr

ENTRYPOINT ["/go/bin/stallarr"]