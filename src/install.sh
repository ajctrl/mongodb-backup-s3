#!/bin/sh
set -eu

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg libgssapi-krb5-2 libsasl2-2 tzdata

case "${TARGETARCH:-$(dpkg --print-architecture)}" in
    amd64) tools_arch=x86_64 ;;
    arm64) tools_arch=arm64 ;;
    *) echo 'Only linux/amd64 and linux/arm64 are supported.' >&2; exit 1 ;;
esac

tools_version=${MONGODB_TOOLS_VERSION:-100.18.0}
tools_name="mongodb-database-tools-ubuntu2404-${tools_arch}-${tools_version}"
tools_dir=$(mktemp -d)
trap 'rm -rf "$tools_dir"' EXIT HUP INT TERM
curl --fail --show-error --silent --location --retry 3 \
    "https://fastdl.mongodb.org/tools/db/${tools_name}.tgz" \
    --output "$tools_dir/tools.tgz"
tar -xzf "$tools_dir/tools.tgz" -C "$tools_dir"
install -m 0755 "$tools_dir/$tools_name/bin/mongodump" /usr/local/bin/mongodump
install -m 0755 "$tools_dir/$tools_name/bin/mongorestore" /usr/local/bin/mongorestore
mkdir -p /usr/share/licenses/mongodb-database-tools
find "$tools_dir/$tools_name" -maxdepth 1 -type f \
    -exec cp '{}' /usr/share/licenses/mongodb-database-tools/ \;
mongodump --version
mongorestore --version

apt-get purge -y curl
rm -rf /var/lib/apt/lists/*
