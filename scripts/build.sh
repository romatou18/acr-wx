#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

set -e

echo "🔨 Building Go Serverless Executable..."

# Clean previous builds
rm -rf functions/
mkdir -p functions/

# Cross-compile for Netlify's Linux environment (AWS Lambda)
# CGO_ENABLED=0 ensures a static binary with no external C dependencies
echo "⚙️  Compiling for linux/amd64..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o functions/weather-bot ./cmd/bot

echo "✅ Build complete! Binary generated at: functions/weather-bot"

# Optional: Run Netlify Dev to test it locally
# Uncomment the line below if you want the script to instantly launch a local test server
# netlify dev
