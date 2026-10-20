# Built from the binaries scripts/build.sh puts in dist/ (docker build dist/).
FROM alpine:3.24
ARG VERSION=dev
ARG REVISION=
LABEL org.opencontainers.image.title="The Repo Depot" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 65532 depot && mkdir /data && chown depot /data
COPY repodepot /usr/local/bin/repodepot
COPY agents /agents
USER depot
ENV REPODEPOT_DATA=/data REPODEPOT_LISTEN=:8470 REPODEPOT_AGENTS=/agents
VOLUME /data
EXPOSE 8470
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8470/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/repodepot"]
