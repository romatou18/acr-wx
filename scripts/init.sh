#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

set -e # Exit immediately if a command exits with a non-zero status

echo "🏔️  Initializing acr-wx Garming weather text bridge..."

# 1. Tidy Go Modules
rm -f go.mod go.sum
echo "📦 Downloading Go dependencies..."
go mod init acr-wx
go get github.com/tursodatabase/libsql-client-go/libsql
go get github.com/aws/aws-lambda-go/lambda
go get github.com/emersion/go-imap
go get github.com/emersion/go-message/mail
go get github.com/emersion/go-imap/client
go get github.com/PuerkitoBio/goquery

go mod tidy

# 2. Check for Netlify CLI
if ! command -v netlify &> /dev/null; then
    echo "⚠️  Netlify CLI not found. Installing via npm..."
    npm install -g netlify-cli
else
    echo "✅ Netlify CLI is already installed."
fi

# 3. Create Local .env file for testing
if [ ! -f .env ]; then
    echo "📝 Creating local .env file..."
    cat <<EOT >> .env
TURSO_DB_URL=libsql://your-database-name.turso.io
TURSO_AUTH_TOKEN=your_turso_token_here
EMAIL_USER=your-email@gmail.com
EMAIL_PASS=your-app-password
EOT
    echo "✅ Created .env template. Please fill in your actual credentials."
else
    echo "✅ .env file already exists."
fi

echo "🚀 Initialization complete!"
