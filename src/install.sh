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

tools_version=${MONGODB_TOOLS_VERSION:-100.20.1}
. /etc/os-release
ubuntu_release=$(printf '%s' "$VERSION_ID" | tr -d '.')
tools_name="mongodb-database-tools-ubuntu${ubuntu_release}-${tools_arch}-${tools_version}"
tools_dir=$(mktemp -d)
trap 'rm -rf "$tools_dir"' EXIT HUP INT TERM
curl --fail --show-error --silent --location --retry 3 \
    "https://fastdl.mongodb.org/tools/db/${tools_name}.tgz" \
    --output "$tools_dir/$tools_name.tgz"
# Official MongoDB CLI Tools release key, pinned in the build context.
curl --fail --show-error --silent --location --retry 3 \
    "https://fastdl.mongodb.org/tools/db/${tools_name}.tgz.sig" \
    --output "$tools_dir/tools.sig"
gpg --batch --dearmor --output "$tools_dir/release.gpg" /mongodb-tools.asc
gpgv --keyring "$tools_dir/release.gpg" "$tools_dir/tools.sig" "$tools_dir/$tools_name.tgz"
tar -xzf "$tools_dir/$tools_name.tgz" -C "$tools_dir"
install -m 0755 "$tools_dir/$tools_name/bin/mongodump" /usr/local/bin/mongodump
install -m 0755 "$tools_dir/$tools_name/bin/mongorestore" /usr/local/bin/mongorestore
mkdir -p /usr/share/licenses/mongodb-database-tools
find "$tools_dir/$tools_name" -maxdepth 1 -type f \
    -exec cp '{}' /usr/share/licenses/mongodb-database-tools/ \;
mongodump --version
mongorestore --version

apt-get purge -y curl
rm -rf /var/lib/apt/lists/*
