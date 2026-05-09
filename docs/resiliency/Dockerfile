FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
	go build -tags kqueue -trimpath -o /out/buckit .

FROM minio/minio:latest

RUN chmod -R 777 /usr/bin

COPY --from=builder /out/buckit /usr/bin/buckit
COPY dockerscripts/docker-entrypoint.sh /usr/bin/docker-entrypoint.sh

ENTRYPOINT ["/usr/bin/docker-entrypoint.sh"]

VOLUME ["/data"]

CMD ["buckit"]
