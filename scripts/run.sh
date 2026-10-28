#!/usr/bin/env bash
# Runs paygate locally with settings from .env (no make needed).
#
#   ./scripts/run.sh env    create .env from .env.example (then edit DATABASE_URL)
#   ./scripts/run.sh db     create the database named in DATABASE_URL
#   ./scripts/run.sh        start the app (tables are created/updated on start)
#
# ENV_FILE=other.env ./scripts/run.sh   to use another settings file.
set -euo pipefail
cd "$(dirname "$0")/.."
ENV_FILE=${ENV_FILE:-.env}

load_env() {
  [[ -f "$ENV_FILE" ]] || { echo "missing $ENV_FILE: run './scripts/run.sh env' first"; exit 1; }
  set -a
  # shellcheck disable=SC1090
  . "./$ENV_FILE"
  set +a
}

case "${1:-run}" in
  env)
    if [[ -f .env ]]; then echo ".env already exists"; else cp .env.example .env && echo "created .env: now set DATABASE_URL"; fi
    ;;
  db)
    load_env
    name=$(sed -E 's#.*/([^/?]+)(\?.*)?$#\1#' <<<"$DATABASE_URL")
    admin_url=$(sed -E "s#/${name}(\?|$)#/postgres\1#" <<<"$DATABASE_URL")
    if psql "$admin_url" -tAc "SELECT 1 FROM pg_database WHERE datname = '$name'" | grep -q 1; then
      echo "database $name already exists"
    else
      psql "$admin_url" -c "CREATE DATABASE \"$name\"" && echo "created database $name"
    fi
    ;;
  run)
    load_env
    exec go run ./cmd/paygate
    ;;
  *)
    echo "usage: $0 [env|db|run]"; exit 2
    ;;
esac
