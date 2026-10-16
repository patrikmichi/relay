# The connector image: the static binary on a distroless base, running as a
# non-root user. The image is immutable; updates are a new tag, never the
# process replacing itself. goreleaser puts each platform's binary under
# $TARGETPLATFORM in the build context.
FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/zuano-connector /usr/local/bin/zuano-connector

ENV ZUANO_CONNECTOR_CONTAINER=1 \
    ZUANO_CONNECTOR_CONFIG=/etc/zuano-connector/config.yaml \
    ZUANO_CONNECTOR_STATE_DIR=/var/lib/zuano-connector

# The identity lives here: keep it on a volume.
VOLUME ["/var/lib/zuano-connector"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/zuano-connector"]
CMD ["run"]
