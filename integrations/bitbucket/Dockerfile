ARG AUTOSQL_IMAGE=ghcr.io/stigenai/autosql@sha256:0e748a1c8fbc51c2bfbdf00be13bf0484d5c6f6be8b45612550ec211d4cd0905
FROM ${AUTOSQL_IMAGE} AS autosql
FROM alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
RUN addgroup -S autosql && adduser -S -G autosql autosql
COPY --from=autosql /autosql /usr/local/bin/autosql
COPY pipe.sh /usr/local/bin/autosql-pipe
USER autosql:autosql
ENTRYPOINT ["/usr/local/bin/autosql-pipe"]
