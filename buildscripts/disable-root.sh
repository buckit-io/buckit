#!/bin/bash

set -x

export MINIO_CI_CD=1
killall -9 buckit minio 2>/dev/null || true

rm -rf ${HOME}/tmp/dist

scheme="http"
nr_servers=4

addr="localhost"
args=""
for ((i = 0; i < $((nr_servers)); i++)); do
	args="$args $scheme://$addr:$((9100 + i))/${HOME}/tmp/dist/path1/$i"
done

echo $args

for ((i = 0; i < $((nr_servers)); i++)); do
	(./buckit server --address ":$((9100 + i))" $args >/tmp/log$i.txt 2>&1) &
done

sleep 10s

if [ ! -f ./mc ]; then
	wget --quiet -O ./mc https://dl.minio.io/client/mc/release/linux-amd64/./mc &&
		chmod +x mc
fi

set +e

export MC_HOST_minioadm=http://buckitadmin:buckitadmin@localhost:9100/
./mc ready minioadm

./mc ls minioadm/

./mc admin config set minioadm/ api root_access=off

sleep 3s # let things settle a little

./mc ls minioadm/
if [ $? -eq 0 ]; then
	echo "listing succeeded, 'buckitadmin' was not disabled"
	exit 1
fi

set -e

killall -9 buckit minio 2>/dev/null || true

export MINIO_API_ROOT_ACCESS=on
for ((i = 0; i < $((nr_servers)); i++)); do
	(./buckit server --address ":$((9100 + i))" $args >/tmp/log$i.txt 2>&1) &
done

set +e

./mc ready minioadm/

./mc ls minioadm/
if [ $? -ne 0 ]; then
	echo "listing failed, 'buckitadmin' should be enabled"
	exit 1
fi

killall -9 buckit minio 2>/dev/null || true

rm -rf /tmp/multisitea/
rm -rf /tmp/multisiteb/

echo "Setup site-replication and then disable root credentials"

./buckit server --address 127.0.0.1:9001 "http://127.0.0.1:9001/tmp/multisitea/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9002/tmp/multisitea/data/disterasure/xl{5...8}" >/tmp/sitea_1.log 2>&1 &
./buckit server --address 127.0.0.1:9002 "http://127.0.0.1:9001/tmp/multisitea/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9002/tmp/multisitea/data/disterasure/xl{5...8}" >/tmp/sitea_2.log 2>&1 &

./buckit server --address 127.0.0.1:9003 "http://127.0.0.1:9003/tmp/multisiteb/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9004/tmp/multisiteb/data/disterasure/xl{5...8}" >/tmp/siteb_1.log 2>&1 &
./buckit server --address 127.0.0.1:9004 "http://127.0.0.1:9003/tmp/multisiteb/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9004/tmp/multisiteb/data/disterasure/xl{5...8}" >/tmp/siteb_2.log 2>&1 &

export MC_HOST_sitea=http://buckitadmin:buckitadmin@127.0.0.1:9001
export MC_HOST_siteb=http://buckitadmin:buckitadmin@127.0.0.1:9004

./mc ready sitea
./mc ready siteb

./mc admin replicate add sitea siteb

./mc admin user add sitea foobar foo12345

./mc admin policy attach sitea/ consoleAdmin --user=foobar

./mc admin user info siteb foobar

killall -9 buckit minio 2>/dev/null || true

echo "turning off root access, however site replication must continue"
export MINIO_API_ROOT_ACCESS=off

./buckit server --address 127.0.0.1:9001 "http://127.0.0.1:9001/tmp/multisitea/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9002/tmp/multisitea/data/disterasure/xl{5...8}" >/tmp/sitea_1.log 2>&1 &
./buckit server --address 127.0.0.1:9002 "http://127.0.0.1:9001/tmp/multisitea/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9002/tmp/multisitea/data/disterasure/xl{5...8}" >/tmp/sitea_2.log 2>&1 &

./buckit server --address 127.0.0.1:9003 "http://127.0.0.1:9003/tmp/multisiteb/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9004/tmp/multisiteb/data/disterasure/xl{5...8}" >/tmp/siteb_1.log 2>&1 &
./buckit server --address 127.0.0.1:9004 "http://127.0.0.1:9003/tmp/multisiteb/data/disterasure/xl{1...4}" \
	"http://127.0.0.1:9004/tmp/multisiteb/data/disterasure/xl{5...8}" >/tmp/siteb_2.log 2>&1 &

export MC_HOST_sitea=http://foobar:foo12345@127.0.0.1:9001
export MC_HOST_siteb=http://foobar:foo12345@127.0.0.1:9004

./mc ready sitea
./mc ready siteb

./mc admin user add sitea foobar-admin foo12345

sleep 2s

./mc admin user info siteb foobar-admin
