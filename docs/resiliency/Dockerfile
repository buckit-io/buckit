FROM minio/minio:latest

RUN chmod -R 777 /usr/bin

COPY ./buckit /usr/bin/buckit
COPY dockerscripts/docker-entrypoint.sh /usr/bin/docker-entrypoint.sh

ENTRYPOINT ["/usr/bin/docker-entrypoint.sh"]

VOLUME ["/data"]

CMD ["buckit"]
