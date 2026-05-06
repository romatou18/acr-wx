#!/bin/bash
# scripts/test-email.sh

set -e

echo "📧 Testing Gmail IMAP Setup..."

# Ensure the .env file exists
if [ ! -f .env ]; then
    echo "❌ Error: .env file not found in the root directory."
    echo "Run ./scripts/init.sh first or create the .env manually."
    exit 1
fi

# Safely load variables from .env and export them for the Go run command
# (Ignores comments and empty lines)
export $(grep -v '^#' .env | xargs)

# Run the Go test program
go run ./cmd/test-email/main.go
