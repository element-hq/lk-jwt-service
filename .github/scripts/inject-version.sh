#!/bin/sh

# Replaces the version in Cargo.toml and Cargo.lock of the current directory
# with the version derived from the supplied tag. Fails if the tag is not a
# release tag.
#
# Usage: inject-version.sh <tag>    (e.g. inject-version.sh v1.2.3)

set -eu

if [ $# -ne 1 ] || [ -z "$1" ]; then
  echo "Usage: inject-version.sh <tag>" >&2
  exit 2
fi

tag="$1"

# "v" followed by a semver version with an optional pre-release part. Numeric
# pre-release identifiers must not have leading zeros, which Cargo rejects.
prerelease_id='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
semver_re="^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-$prerelease_id(\.$prerelease_id)*)?\$"
newline='
'

# grep matches per line, so reject multi-line input up front.
case "$tag" in
  *"$newline"*) valid=false ;;
  *) if printf '%s\n' "$tag" | grep -Eq "$semver_re"; then valid=true; else valid=false; fi ;;
esac

if [ "$valid" != true ]; then
  echo "::error::'$tag' is not a release tag of the form vMAJOR.MINOR.PATCH[-PRERELEASE]" >&2
  exit 1
fi

version="${tag#v}"

placeholder='0.0.0-git'
placeholder_re='0\.0\.0-git'

inject() {
  tmp="$(mktemp)"
  sed -E "$2" "$1" > "$tmp"
  if cmp -s "$1" "$tmp"; then
    rm -f "$tmp"
    echo "::error::Did not find the placeholder version $placeholder in $1" >&2
    exit 1
  fi
  # Write through the existing file to keep its mode and owner.
  cat "$tmp" > "$1"
  rm -f "$tmp"
}

# The version key inside the [package] table.
inject Cargo.toml \
  "/^\[package\]\$/,/^\[/ s/^version = \"$placeholder_re\"\$/version = \"$version\"/"

# The version line directly following the package's name line.
inject Cargo.lock \
  "/^name = \"lk-jwt-service\"\$/{
n
s/^version = \"$placeholder_re\"\$/version = \"$version\"/
}"

echo "Successfully injected version $version"
