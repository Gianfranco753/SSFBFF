#!/bin/sh
# Clean script for SSFBFF
# Removes all generated files, binaries, and optionally the generated directory

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$PROJECT_ROOT"

REMOVED_COUNT=0

echo "🧹 Cleaning generated files and binaries..."

# Remove the entire internal/generated/ directory
if [ -d "internal/generated" ]; then
  echo "Removing internal/generated/ directory and all contents..."
  # Count files before removal for accurate reporting
  FILE_COUNT=$(find internal/generated -type f 2>/dev/null | wc -l | tr -d ' ')
  rm -rf "internal/generated"
  if [ "$FILE_COUNT" -gt 0 ]; then
    REMOVED_COUNT=$((REMOVED_COUNT + FILE_COUNT))
  else
    REMOVED_COUNT=$((REMOVED_COUNT + 1))
  fi
  echo "  ✓ Removed: internal/generated/ (directory)"
fi

# Remove generated routes file
if [ -f "cmd/server/routes_gen.go" ]; then
  rm -f "cmd/server/routes_gen.go"
  REMOVED_COUNT=$((REMOVED_COUNT + 1))
  echo "  ✓ Removed: cmd/server/routes_gen.go"
fi

# Remove binaries from project root
BINARIES="apigen transpiler server mockserver"
for binary in $BINARIES; do
  if [ -f "$binary" ]; then
    rm -f "$binary"
    REMOVED_COUNT=$((REMOVED_COUNT + 1))
    echo "  ✓ Removed: $binary"
  fi
done

# Remove test binaries
echo "Removing test binaries..."
find . -name "*.test" -type f -not -path "./.git/*" -not -path "./examples/*" | while read -r testfile; do
  rm -f "$testfile"
  REMOVED_COUNT=$((REMOVED_COUNT + 1))
  echo "  ✓ Removed: $testfile"
done


# Remove coverage files
echo "Removing coverage files..."
for covfile in coverage.out coverage.*.out *.coverprofile profile.cov; do
  if [ -f "$covfile" ]; then
    rm -f "$covfile"
    REMOVED_COUNT=$((REMOVED_COUNT + 1))
    echo "  ✓ Removed: $covfile"
  fi
done

if [ "$REMOVED_COUNT" -eq 0 ]; then
  echo "✅ Nothing to clean - all generated files and binaries are already removed"
else
  echo "✅ Cleaned $REMOVED_COUNT file(s)"
fi

echo ""
echo "To regenerate everything, run:"
echo "  ./scripts/generate-generate-go.sh"
echo "  GOEXPERIMENT=jsonv2 go generate ./internal/generated/"
