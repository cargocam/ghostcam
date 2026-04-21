#!/usr/bin/env bash
# Load secrets from a .env file into Pulumi encrypted config.
# Usage: ./load-secrets.sh [secrets.env]
set -euo pipefail

FILE="${1:-secrets.env}"

if [ ! -f "$FILE" ]; then
  echo "File not found: $FILE" >&2
  echo "Copy secrets.env.example to secrets.env and fill in values." >&2
  exit 1
fi

while IFS= read -r line; do
  # Skip comments and blank lines
  [[ -z "$line" || "$line" =~ ^[[:space:]]*# ]] && continue

  key="${line%%=*}"
  value="${line#*=}"

  # Skip empty values
  [ -z "$value" ] && continue

  echo "Setting $key"
  pulumi config set --secret "$key" "$value"
done < "$FILE"

echo "Done. Run 'pulumi preview' to verify."
