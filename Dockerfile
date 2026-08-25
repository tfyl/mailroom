# The build stage runs on the builder's own architecture and cross-compiles to the target,
# rather than being emulated once per target architecture. Go cross-compiles natively, so
# emulation only ever bought a slower compiler: in release run 32312695295 the amd64 build
# took 117s and the identical arm64 build took 1027s of the same job.
FROM --platform=$BUILDPLATFORM golang:1.20-alpine AS build
WORKDIR /src

# Above the target-architecture arguments on purpose. Module contents do not depend on
# GOARCH, so leaving this layer architecture-independent makes it one download shared by
# every target instead of one download per target.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Declared here rather than at the top of the stage: an ARG invalidates every layer below
# it, and these are the values that differ per target. buildx supplies both.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
# CGO off keeps the binary static: the SQLite driver is pure Go, so there is nothing to link
# against and the runtime image needs no libc. It is also what makes the cross-compile free,
# since a cgo build would need a C toolchain for the target architecture.
#
# The build cache is a mount rather than a layer because its contents are worthless to the
# published image and it is keyed per architecture by Go itself. It does not survive a CI
# runner, where it costs nothing; locally it turns a rebuild after a one-line edit into a
# few seconds.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/tfyl/mailroom/internal/mcp.Version=${VERSION}" \
    -o /mailroom ./cmd/mailroom

# Staged here so the runtime stage can copy it with the right ownership. Distroless has no
# shell, so there is no mkdir available after the FROM below.
RUN mkdir -p /staged-data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /mailroom /mailroom

# The licences of the Go modules compiled into that binary. MIT, BSD and Apache-2.0 all ask
# for their copyright and permission notices to accompany a copy of the software, and an image
# is a copy; Go puts none of them in a binary by itself.
#
# The binary embeds the same file and `mailroom notices` prints it, so this is not what makes
# the image compliant — it is what makes the notices readable without running anything, which
# is the difference between `docker cp` on a created container and needing one that starts.
# 240 KB, against a compressed image of about twenty megabytes.
COPY --from=build /src/internal/notices/NOTICES.md /NOTICES.md

# /data must exist in the image, owned by the user the container runs as. Docker seeds a new
# named volume from the image path *including its ownership*, so without this the volume
# arrives root-owned and SQLite fails with "unable to open database file" — an error that
# says nothing about permissions and appears only at runtime.
#
# A bind mount takes the host directory's ownership instead and ignores this entirely, so a
# host path must be chowned to 65532 by whoever mounts it. See deploy/docker-compose.yml.
COPY --from=build --chown=nonroot:nonroot /staged-data /data

# State lives here. Mount a volume, or every linked mailbox disappears with the container.
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/mailroom"]
