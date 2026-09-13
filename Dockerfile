# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26
ARG UBUNTU_VERSION=24.04

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/mongodb-backup-s3 ./cmd/mongodb-backup-s3

FROM ubuntu:${UBUNTU_VERSION} AS tools
ARG TARGETARCH
ARG MONGODB_TOOLS_VERSION=100.18.0
COPY src/install.sh /install.sh
RUN TARGETARCH="${TARGETARCH}" MONGODB_TOOLS_VERSION="${MONGODB_TOOLS_VERSION}" \
    sh /install.sh && rm /install.sh
COPY src/runtime-rootfs.sh /runtime-rootfs.sh
RUN sh /runtime-rootfs.sh

FROM scratch
COPY --from=tools /runtime-rootfs/ /
COPY --from=build /out/mongodb-backup-s3 /usr/local/bin/mongodb-backup-s3

ENV PATH=/usr/local/bin:/usr/bin:/bin \
    HOME=/root \
    BACKUP_MODE=full \
    MONGODB_URI="" \
    MONGODB_URI_FILE="" \
    MONGODB_DATABASE="" \
    MONGODB_DATABASES="" \
    MONGODB_BACKUP_ALL=false \
    MONGODB_DATABASES_EXCLUDE="" \
    MONGODB_READ_PREFERENCE=primary \
    MONGODB_TLS_CA_FILE="" \
    MONGODB_CONFIG_DIR="" \
    MONGODB_PARALLEL_COLLECTIONS=4 \
    MONGODB_RESTORE_DROP=false \
    S3_ACCESS_KEY_ID="" \
    S3_SECRET_ACCESS_KEY="" \
    S3_SESSION_TOKEN="" \
    S3_BUCKET="" \
    S3_REGION=us-west-1 \
    S3_PREFIX=backup \
    S3_ENDPOINT="" \
    S3_UPLOAD_PART_SIZE_MB=8 \
    SCHEDULE="" \
    PASSPHRASE="" \
    BACKUP_KEEP_DAYS="" \
    BACKUP_FILENAME_MODE=timestamp

COPY --chmod=755 src/run.sh src/backup.sh src/restore.sh /
COPY LICENSE LICENSE.txt /usr/share/licenses/mongodb-backup-s3/
CMD ["mongodb-backup-s3", "run"]
