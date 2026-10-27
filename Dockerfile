# syntax=docker/dockerfile:1
# The build stage runs natively and cross-compiles (no QEMU for arm64).
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /out/kaku ./cmd/kaku

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata su-exec && adduser -D -u 10001 app && mkdir /data && chown app /data
COPY --from=build /out/kaku /usr/local/bin/kaku
COPY --chmod=0755 deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV DATA_DIR=/data LISTEN=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["serve"]
