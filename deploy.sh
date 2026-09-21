#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

for command_name in go pm2; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "$command_name is required and was not found on PATH." >&2
    exit 1
  fi
done

if [ ! -f config.json ]; then
  echo "Missing private config.json; copy it to this server before deploying." >&2
  exit 1
fi

go build -o twitch-watcher.new .
mv -f twitch-watcher.new twitch-watcher

if pm2 describe twitch-watcher >/dev/null 2>&1; then
  pm2 restart twitch-watcher --update-env
else
  pm2 start ./twitch-watcher --name twitch-watcher --interpreter none --cwd "$PWD"
fi
pm2 save
pm2 status twitch-watcher
