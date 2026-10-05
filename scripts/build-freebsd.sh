#!/bin/sh
# Cross-build a genuine FreeBSD shared library on Linux. No VM is booted.
set -eu
if [ "$(uname -s)" != Linux ]; then
  echo 'This cross-build runs on Linux. On native FreeBSD/amd64, use gmake build.' >&2
  exit 1
fi
for tool in go make clang ld.lld curl tar sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Missing cross-build dependency: $tool" >&2; exit 1; }
done
repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"
cache=${FREEBSD_CACHE_DIR:-"$repo/.cache/freebsd"}
mkdir -p "$cache"
cache=$(CDPATH='' cd -- "$cache" && pwd)
release=14.4-RELEASE
base_sha256=769f60a6eea2938ad6b7943cfbcc17dfaf7d1f7ba59a32b869a00349302df853
archive="$cache/base-$release-amd64.txz"
sysroot="$cache/sysroot-$release-amd64"
staging=''
download=''
cleanup() {
  [ -z "$staging" ] || rm -rf -- "$staging"
  [ -z "$download" ] || rm -f -- "$download"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

if [ ! -f "$sysroot/.verified-base-sha256" ]; then
  if [ -e "$sysroot" ]; then
    echo 'Refusing to overwrite an unverified sysroot; select a fresh FREEBSD_CACHE_DIR.' >&2
    exit 1
  fi
  if ! printf '%s  %s\n' "$base_sha256" "$archive" | sha256sum --check --status 2>/dev/null; then
    download=$(mktemp "$cache/.base-download.XXXXXX")
    # Old releases may move to the official archive; the pinned hash is unchanged.
    if ! curl --fail --silent --show-error --location --retry 3 --connect-timeout 20 --max-time 600 \
      "https://download.freebsd.org/releases/amd64/amd64/$release/base.txz" --output "$download"; then
      curl --fail --silent --show-error --location --retry 3 --connect-timeout 20 --max-time 600 \
        "https://archive.freebsd.org/old-releases/amd64/amd64/$release/base.txz" --output "$download"
    fi
    printf '%s  %s\n' "$base_sha256" "$download" | sha256sum --check --status
    mv "$download" "$archive"
    download=''
  fi
  staging=$(mktemp -d "$cache/.sysroot.XXXXXX")
  # Extract only compiler headers, startup objects, and target libraries.
  tar --extract --xz --file "$archive" --directory "$staging" \
    --no-same-owner --no-same-permissions --warning=no-unknown-keyword \
    ./usr/include ./usr/lib ./lib ./libexec
  test -f "$staging/usr/include/stdlib.h"
  test -f "$staging/usr/lib/crtbeginS.o"
  printf '%s\n' "$base_sha256" > "$staging/.verified-base-sha256"
  mv "$staging" "$sysroot"
  staging=''
fi
if [ "$(cat "$sysroot/.verified-base-sha256")" != "$base_sha256" ]; then
  echo 'Cached FreeBSD sysroot does not match the pinned release.' >&2
  exit 1
fi

(
  export GOOS=freebsd GOARCH=amd64 CGO_ENABLED=1 FREEBSD_SYSROOT="$sysroot"
  # Go parses quoted compiler command strings, including paths with spaces.
  export CC="\"$repo/scripts/freebsd-cc.sh\""
  make build VERSION="${VERSION:-0.1.1}" REPOSITORY="${REPOSITORY:-UNCONFIGURED}"
  # Compile OS-specific test code without pretending to execute it on Linux.
  mkdir -p "$cache/test-binaries"
  for package in $(go list ./...); do
    name=$(printf '%s' "$package" | tr '/.' '__')
    go test -c -o "$cache/test-binaries/$name.test" "$package"
  done

)

# make build already ran cmd/checklib against dist/freebsd/amd64/github-copilot.so,
# confirming the FreeBSD ELF ABI and the native plugin entry point.
echo 'FreeBSD test binaries were cross-compiled, not executed.'
