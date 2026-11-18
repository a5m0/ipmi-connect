#!/usr/bin/env bash
set -euo pipefail

export COPYFILE_DISABLE=1

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  echo "Usage: $0 <version>" >&2
  exit 1
fi
VERSION="${VERSION#v}"

# Determine repo root and output directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DIST_DIR="$REPO_ROOT/dist"

rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
)

echo "==> Building ipmi-connect v${VERSION} for Linux and macOS (x86 & ARM)..."

for target in "${TARGETS[@]}"; do
  os="${target%%/*}"
  arch="${target##*/}"
  echo "--> Building ${os}/${arch}..."

  TMP_BUILD="$DIST_DIR/build_${os}_${arch}"
  mkdir -p "$TMP_BUILD"

  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o "$TMP_BUILD/ipmi-connect" \
    "$REPO_ROOT/cmd/ipmi-connect"

  files=(ipmi-connect)
  # Linux archives also carry the native-window build (Wails; GTK4 and WebKitGTK 6.0 are loaded
  # at run time, so it still needs no cgo). The browser build stays for CLI use and machines
  # without those libraries.
  if [[ "$os" == "linux" ]]; then
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
      -trimpath \
      -tags desktop,purego,production \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o "$TMP_BUILD/ipmi-connect-desktop" \
      "$REPO_ROOT/cmd/ipmi-connect"
    files+=(ipmi-connect-desktop)
  fi

  cp "$REPO_ROOT/LICENSE" "$TMP_BUILD/"
  cp "$REPO_ROOT/NOTICE" "$TMP_BUILD/"
  cp "$REPO_ROOT/README.md" "$TMP_BUILD/"

  ARCHIVE_NAME="ipmi-connect_${VERSION}_${os}_${arch}.tar.gz"
  tar -czf "$DIST_DIR/$ARCHIVE_NAME" -C "$TMP_BUILD" "${files[@]}" LICENSE NOTICE README.md
  rm -rf "$TMP_BUILD"
  echo "    Created $ARCHIVE_NAME"
done

# Flatpak bundles built by the release workflow (flatpak.yml) are collected here.
if compgen -G "$REPO_ROOT/flatpak-dist/*.flatpak" >/dev/null; then
  echo "==> Adding Flatpak bundles..."
  for f in "$REPO_ROOT"/flatpak-dist/*.flatpak; do
    case "$(basename "$f")" in
      ipmi-connect_"${VERSION}"_linux_*.flatpak) cp "$f" "$DIST_DIR/" ;;
      *) echo "Flatpak bundle $f is not for version ${VERSION}" >&2; exit 1 ;;
    esac
  done
fi

echo "==> Generating the SBOM (CycloneDX)..."
(cd "$REPO_ROOT" && go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0 \
  app -json -licenses -main cmd/ipmi-connect -output "$DIST_DIR/ipmi-connect_${VERSION}.cdx.json")
# The desktop build links Wails and its dependencies, so it gets its own SBOM.
(cd "$REPO_ROOT" && GOFLAGS="-tags=desktop,purego,production" go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0 \
  app -json -licenses -main cmd/ipmi-connect -output "$DIST_DIR/ipmi-connect-desktop_${VERSION}.cdx.json")

echo "==> Generating sha256sums.txt..."
(
  cd "$DIST_DIR"
  files=(*.tar.gz *.cdx.json)
  if compgen -G "*.flatpak" >/dev/null; then files+=(*.flatpak); fi
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${files[@]}" > sha256sums.txt
  else
    shasum -a 256 "${files[@]}" > sha256sums.txt
  fi
)

echo "==> Release assets created in dist/:"
ls -lh "$DIST_DIR"
