FROM golang:1.22.1-alpine as build

COPY *.go /go/src/github.com/nanoandrew4/ngcplogs/
COPY go.mod /go/src/github.com/nanoandrew4/ngcplogs/
COPY go.sum /go/src/github.com/nanoandrew4/ngcplogs/

RUN cd /go/src/github.com/nanoandrew4/ngcplogs && go get && go build --ldflags '-extldflags "-static"' -o /usr/bin/ngcplogs

FROM alpine:3.19.1
COPY --from=build /usr/bin/ngcplogs usr/bin

WORKDIR /usr/bin
ENTRYPOINT [ "/usr/bin/ngcplogs" ]