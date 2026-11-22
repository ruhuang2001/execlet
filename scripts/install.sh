#!/bin/sh
set -eu

main() {
  for command in curl tar uname mktemp install; do
    command -v "$command" >/dev/null 2>&1 || { echo "Required command not found: $command" >&2; exit 1; }
  done
  case "$(uname -s)" in
    Darwin) platform=darwin ;;
    Linux) platform=linux ;;
    *) echo "Supported platforms: macOS and Linux. Windows is not supported." >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac

  repo=https://github.com/ruhuang2001/tiny-sandbox
  release=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
  case "$release" in
    "$repo/releases/tag/v"*) tag=${release##*/} ;;
    *) echo "Could not find the latest release." >&2; exit 1 ;;
  esac
  destination=${1:-"$HOME/tiny-sandbox-quickstart"}
  work=$(mktemp -d "${TMPDIR:-/tmp}/tiny-sandbox-install.XXXXXX")
  trap 'rm -rf "$work"' 0
  trap 'exit 130' INT
  trap 'exit 143' TERM

  archive="tiny-sandbox_${tag#v}_${platform}_${arch}.tar.gz"
  echo "Downloading tiny-sandbox $tag ($platform/$arch)..."
  curl -fL --retry 3 "$repo/releases/download/$tag/$archive" -o "$work/$archive"
  curl -fsSL --retry 3 "$repo/releases/download/$tag/checksums.txt" -o "$work/checksums.txt"
  expected=$(awk -v name="$archive" '$2 == name { print $1 }' "$work/checksums.txt")
  [ -n "$expected" ] || { echo "Checksum not found for $archive." >&2; exit 1; }
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$work/$archive" | awk '{ print $1 }')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$work/$archive" | awk '{ print $1 }')
  else
    echo "Required checksum tool not found: sha256sum or shasum" >&2
    exit 1
  fi
  [ "$actual" = "$expected" ] || { echo "Checksum mismatch for $archive." >&2; exit 1; }
  tar -xzf "$work/$archive" -C "$work" sandboxd sandboxctl
  mkdir -p "$destination"
  install -m 755 "$work/sandboxd" "$destination/sandboxd"
  install -m 755 "$work/sandboxctl" "$destination/sandboxctl"
  echo "Installed sandboxd and sandboxctl in $destination"
  echo "Next: https://github.com/ruhuang2001/tiny-sandbox/blob/main/docs/quickstart.md"
}

main "$@"
