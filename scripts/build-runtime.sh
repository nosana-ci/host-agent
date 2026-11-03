#!/bin/sh
# Builds the runtime bundle: a relocatable, statically linked podman with its helpers, repackaged
# from a GPG-verified release of github.com/mgoltzsche/podman-static.
#
#   scripts/build-runtime.sh 5.8.8 [out-dir]
#
# Layout of nosana-runtime-<version>-linux-amd64.tar.gz:
#   bin/          podman crun runc pasta pasta.avx2 fuse-overlayfs fusermount3
#   lib/podman/   conmon netavark aardvark-dns catatonit rootlessport
#   etc/          seccomp.json policy.json   (containers.conf is written by the agent)
set -eu

VERSION="${1:?usage: build-runtime.sh <podman-version> [out-dir]}"
OUT="${2:-bin}"
SIGNING_KEY=0CCF102C4F95D89E583FF1D4F8B5AF50344BB503 # podman-static release key
URL="https://github.com/mgoltzsche/podman-static/releases/download/v${VERSION}/podman-linux-amd64.tar.gz"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

echo "• downloading podman-static v${VERSION}"
curl -fsSL -o "$WORK/podman.tar.gz" "$URL"
curl -fsSL -o "$WORK/podman.tar.gz.asc" "$URL.asc"

echo "• verifying signature"
export GNUPGHOME="$WORK/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --keyserver hkps://keyserver.ubuntu.com --recv-keys "$SIGNING_KEY"
gpg --batch --verify "$WORK/podman.tar.gz.asc" "$WORK/podman.tar.gz"

echo "• repackaging"
mkdir -p "$WORK/src" "$WORK/bundle/bin" "$WORK/bundle/lib/podman" "$WORK/bundle/etc"
tar -xzf "$WORK/podman.tar.gz" -C "$WORK/src"
S="$WORK/src/podman-linux-amd64"
for b in podman crun runc pasta pasta.avx2 fuse-overlayfs fusermount3; do
  cp "$S/usr/local/bin/$b" "$WORK/bundle/bin/"
done
cp "$S"/usr/local/lib/podman/* "$WORK/bundle/lib/podman/"
cp "$S/etc/containers/seccomp.json" "$S/etc/containers/policy.json" "$WORK/bundle/etc/"

TARBALL="$OUT/nosana-runtime-${VERSION}-linux-amd64.tar.gz"
tar -czf "$TARBALL" -C "$WORK/bundle" .
echo "• wrote $TARBALL ($(du -h "$TARBALL" | cut -f1))"
