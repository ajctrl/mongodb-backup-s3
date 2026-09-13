#!/bin/sh
# Assemble the runtime from Ubuntu's binaries and their matching libraries.
# Keep the official MongoDB tools on glibc without shipping the build OS.
set -eu

root=/runtime-rootfs
mkdir -p "$root"

copy_file() {
    cp -L --parents "$1" "$root"
}

copy_binary() {
    copy_file "$1"
    dependencies=$(ldd "$1")
    if printf '%s\n' "$dependencies" | grep -q 'not found'; then
        printf 'Missing runtime dependency for %s\n%s\n' "$1" "$dependencies" >&2
        exit 1
    fi
    # ldd includes transitive dependencies and the architecture-specific loader.
    for library in $(printf '%s\n' "$dependencies" | awk '/=> \// {print $3} /^[[:space:]]*\// {print $1}'); do
        copy_file "$library"
    done
}

for binary in /usr/local/bin/mongodump /usr/local/bin/mongorestore \
    /usr/bin/gpg /usr/bin/gpg-agent /usr/bin/gpgconf /bin/sh; do
    copy_binary "$binary"
done

# These modules/helpers are loaded at runtime and are not listed by ldd.
find /usr/lib \( -type f -o -type l \) \( -path '*/krb5/plugins/*' -o -path '*/sasl2/*' \
    -o -name 'libsasl2.so.*' -o -name 'libnss_dns.so.*' -o -name 'libnss_files.so.*' \
    -o -path '/usr/lib/gnupg/*' \) > /runtime-modules
while IFS= read -r module; do
    copy_binary "$module"
done < /runtime-modules

for file in /etc/ssl/certs/ca-certificates.crt /etc/nsswitch.conf \
    /etc/passwd /etc/group /etc/os-release; do
    copy_file "$file"
done

mkdir -p "$root/usr/share" "$root/usr/share/doc"
cp -a /usr/share/zoneinfo /usr/share/common-licenses /usr/share/licenses "$root/usr/share/"
# Preserve upstream package copyright notices, including symlinked notices.
find -L /usr/share/doc -name copyright -type f > /runtime-copyrights
while IFS= read -r notice; do
    copy_file "$notice"
done < /runtime-copyrights
if [ -d /etc/gnupg ]; then
    cp -a /etc/gnupg "$root/etc/"
fi

mkdir -p "$root/tmp" "$root/var/tmp" "$root/run" "$root/root"
chmod 1777 "$root/tmp" "$root/var/tmp"
chmod 700 "$root/root"
