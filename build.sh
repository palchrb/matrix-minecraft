#!/bin/sh
set -e

MAUTRIX_VERSION=$(grep 'maunium.net/go/mautrix ' go.mod | awk '{ print $2 }' | head -n1)
GIT_TAG=$(git describe --exact-match --tags 2>/dev/null || true)
GIT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -Iseconds)

GO_LDFLAGS="-s -w \
  -X main.Tag=${GIT_TAG} \
  -X main.Commit=${GIT_COMMIT} \
  -X 'main.BuildTime=${BUILD_TIME}' \
  -X 'maunium.net/go/mautrix.GoModVersion=${MAUTRIX_VERSION}'"

echo "Building matrix-minecraft (mautrix-go ${MAUTRIX_VERSION}, commit ${GIT_COMMIT})..."
go build -ldflags="${GO_LDFLAGS}" -o matrix-minecraft ./cmd/matrix-minecraft "$@"
echo "Done: ./matrix-minecraft"
