FROM minio/minio:latest

ARG TARGETARCH
ARG RELEASE

RUN chmod -R 777 /usr/bin

COPY ./buckit-${TARGETARCH}.${RELEASE} /usr/bin/buckit
COPY ./buckit-${TARGETARCH}.${RELEASE}.minisig /usr/bin/buckit.minisig
COPY ./buckit-${TARGETARCH}.${RELEASE}.sha256sum /usr/bin/buckit.sha256sum

COPY dockerscripts/docker-entrypoint.sh /usr/bin/docker-entrypoint.sh

ENTRYPOINT ["/usr/bin/docker-entrypoint.sh"]

VOLUME ["/data"]

CMD ["buckit"]
