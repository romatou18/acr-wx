#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

set -e

echo "🚀 Preparing to deploy to Netlify..."

# 1. Always build a fresh binary before deploying
echo "Running build script..."
./scripts/build.sh

# 2. Deploy to Netlify
# --build: tells Netlify to respect the netlify.toml file
# --prod: pushes directly to production (bypassing Draft previews)
echo "☁️  Pushing to Netlify Edge..."
netlify deploy --build --prod

echo "🏔️  Deployment Successful! Your Alpine Bridge is live."
