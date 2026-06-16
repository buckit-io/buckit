FROM registry.access.redhat.com/ubi9/ubi-micro:latest

LABEL name="Buckit" \
      vendor="Buckit IO" \
      maintainer="Buckit IO" \
      summary="Buckit is a high-performance, S3-compatible object storage server." \
      description="Buckit is a high-performance, S3-compatible object storage solution released under the GNU AGPL v3.0 license."

COPY buckit /usr/bin/buckit
COPY dockerscripts/docker-entrypoint.sh /usr/bin/docker-entrypoint.sh
COPY CREDITS /licenses/CREDITS
COPY LICENSE /licenses/LICENSE

RUN chmod +x /usr/bin/buckit /usr/bin/docker-entrypoint.sh

EXPOSE 9000
VOLUME ["/data"]

ENTRYPOINT ["/usr/bin/docker-entrypoint.sh"]
CMD ["buckit"]
