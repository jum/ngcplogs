FROM --platform=$BUILDPLATFORM golang:1.22.1-alpine as build

WORKDIR /goapp
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg go mod download
COPY *.go ./
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o ngcplogs .

FROM alpine:3.19.1
COPY --from=build /goapp/ngcplogs usr/bin

WORKDIR /usr/bin
ENTRYPOINT [ "/usr/bin/ngcplogs" ]
