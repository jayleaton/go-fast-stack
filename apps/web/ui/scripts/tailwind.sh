#!/usr/bin/env bash
# Downloads the pinned Tailwind CSS standalone CLI into apps/web/ui/bin/ (gitignored)
# and verifies its SHA-256 before making it executable. Safe to re-run.
# Usage: apps/web/ui/scripts/tailwind.sh   -> prints the binary path
set -euo pipefail

VERSION=4.3.3
# From https://github.com/tailwindlabs/tailwindcss/releases/download/v${VERSION}/sha256sums.txt
declare -A SHA256=(
  [linux-x64]=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
  [linux-arm64]=55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195
  [macos-x64]=7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1
  [macos-arm64]=cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d
)

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=macos ;;
  *) echo "tailwind.sh: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=x64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "tailwind.sh: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac
platform="$os-$arch"
want=${SHA256[$platform]}

bin_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin"
bin="$bin_dir/tailwindcss-v$VERSION"

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

if [[ -x "$bin" && "$(sha "$bin")" == "$want" ]]; then
  echo "$bin"
  exit 0
fi

mkdir -p "$bin_dir"
tmp="$(mktemp "$bin_dir/.download.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
curl -fsSL -o "$tmp" "https://github.com/tailwindlabs/tailwindcss/releases/download/v$VERSION/tailwindcss-$platform"
got="$(sha "$tmp")"
if [[ "$got" != "$want" ]]; then
  echo "tailwind.sh: checksum mismatch for tailwindcss-$platform: got $got want $want" >&2
  exit 1
fi
chmod +x "$tmp"
mv "$tmp" "$bin"
echo "$bin"
