#!/bin/sh
set -eu

die() {
  printf '%s\n' "$*" >&2
  exit 1
}

fetch() {
  curl -q -fsSL --retry 3 --proto '=https' --proto-redir '=https' "$@"
}

checksum_for() {
  LC_ALL=C awk -v name="$1" '
    $2 == name || $2 == "*" name {
      count++
      hash = $1
      if (NF != 2) invalid = 1
    }
    END {
      if (!count) exit 0
      if (count != 1 || invalid || length(hash) != 64 || hash ~ /[^0-9a-fA-F]/) exit 1
      print tolower(hash)
    }
  ' "$work/checksums.txt"
}

main() {
  for command in curl tar uname mktemp install awk; do
    command -v "$command" >/dev/null 2>&1 || die "Required command not found: $command"
  done
  case "$(uname -s)" in
    Darwin) platform=darwin ;;
    Linux) platform=linux ;;
    *) die "Supported platforms: macOS and Linux. Windows is not supported." ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *) die "Unsupported architecture: $(uname -m)" ;;
  esac

  repo=https://github.com/ruhuang2001/execlet
  release=$(fetch -o /dev/null -w '%{url_effective}' "$repo/releases/latest")
  case "$release" in
    "$repo/releases/tag/"*) tag=${release#"$repo/releases/tag/"} ;;
    *) die "Latest release URL is not a release tag on $repo." ;;
  esac
  printf '%s\n' "$tag" | LC_ALL=C awk '
    END { exit !(NR == 1 && $0 ~ /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/) }
  ' || die "Invalid version tag in latest release URL."

  destination=${1:-"$HOME/execlet-quickstart"}
  work=$(mktemp -d "${TMPDIR:-/tmp}/execlet-install.XXXXXX")
  trap 'rm -rf "$work"' 0
  trap 'exit 130' INT
  trap 'exit 143' TERM

  assets="$repo/releases/download/$tag"
  fetch "$assets/checksums.txt" -o "$work/checksums.txt"
  suffix="${tag#v}_${platform}_${arch}.tar.gz"
  archive="execlet_$suffix"
  daemon=execletd
  cli=execletctl
  expected=$(checksum_for "$archive") || die "Invalid checksum entry for $archive."
  if [ -z "$expected" ]; then
    case "$tag" in
      v0.0.1|v0.0.2|v0.0.3)
        archive="tiny-sandbox_$suffix"
        daemon=sandboxd
        cli=sandboxctl
        expected=$(checksum_for "$archive") || die "Invalid checksum entry for $archive."
        ;;
    esac
  fi
  [ -n "$expected" ] || die "Checksum not found for $archive."

  echo "Downloading execlet $tag ($platform/$arch)..."
  fetch "$assets/$archive" -o "$work/$archive"
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum < "$work/$archive")
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 < "$work/$archive")
  else
    die "Required checksum tool not found: sha256sum or shasum"
  fi
  [ "${actual%% *}" = "$expected" ] || die "Checksum mismatch for $archive."
  tar -xzf "$work/$archive" -C "$work" "$daemon" "$cli"
  mkdir -p "$destination"
  destination=$(CDPATH= cd -- "$destination" && pwd -P)
  install -m 755 "$work/$daemon" "$destination/execletd"
  install -m 755 "$work/$cli" "$destination/execletctl"
  echo "Installed execletd and execletctl in $destination"
  if [ "$daemon" = sandboxd ]; then
    echo "Installed historical tiny-sandbox $tag under new filenames; it still has the old behavior." >&2
    printf 'In both terminals use this directory: %s\n' "$destination" >&2
    echo 'Skip the tagged install step. Run ./execletd instead of ./sandboxd in terminal A.' >&2
    echo 'In terminal B use ./execletctl instead of sandboxctl (including in printed cleanup hints).' >&2
  fi
  echo "Next: $repo/blob/$tag/docs/quickstart.md"
}

main "$@"
