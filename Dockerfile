FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY internal ./internal
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/control-plane ./cmd/control-plane

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app && apk add --no-cache ca-certificates && mkdir /data && chown app:app /data
COPY --from=build /out/control-plane /control-plane
USER app
EXPOSE 8080
ENTRYPOINT ["/control-plane"]
