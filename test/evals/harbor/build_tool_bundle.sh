#!/usr/bin/env bash
set -euo pipefail

# Build the narrow, reproducible evaluation capability profile. mise and
# lightpanda are deliberately absent: their runtime trees are not part of
# minimal task images, so claiming them would make the score describe a
# fictional Stella.
output=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    *) echo "usage: $0 --output DIR" >&2; exit 2 ;;
  esac
done
[[ -n "$output" ]] || { echo "--output is required" >&2; exit 2; }
rm -rf "$output"
mkdir -p "$output/bin" "$output/skills"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
fetch() { local url="$1" sha="$2" file="$3"; curl --fail --location --silent --show-error "$url" -o "$work/$file"; echo "$sha  $work/$file" | sha256sum -c -; }
fetch https://github.com/BurntSushi/ripgrep/releases/download/14.1.1/ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz 4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e rg.tar.gz
fetch https://github.com/sharkdp/fd/releases/download/v10.2.0/fd-v10.2.0-x86_64-unknown-linux-musl.tar.gz d9bfa25ec28624545c222992e1b00673b7c9ca5eb15393c40369f10b28f9c932 fd.tar.gz
tar -xzf "$work/rg.tar.gz" -C "$work"; tar -xzf "$work/fd.tar.gz" -C "$work"
install -m 0755 "$work"/ripgrep-*/rg "$output/bin/rg"
install -m 0755 "$work"/fd-*/fd "$output/bin/fd"
root="$(cd "$(dirname "$0")/../../.." && pwd)"
cp -R "$root/plugins/core/skills/stella" "$output/skills/stella"
(cd "$output" && find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$output/MANIFEST.sha256"
sha256sum "$output/MANIFEST.sha256" | awk '{print $1}' > "$output/capability_profile.sha256"
