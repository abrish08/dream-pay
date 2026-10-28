#!/usr/bin/env bash
# Runs paygate locally with settings from .env (no make needed).
#
#   ./scripts/run.sh env    create .env from .env.example with generated secrets (then edit DATABASE_URL)
#   ./scripts/run.sh db     create the database named in DATABASE_URL
#   ./scripts/run.sh        start the app (tables are created/updated on start)
#   ./scripts/run.sh demo   run the demo walkthrough against it (uses the admin from .env)
#
# ENV_FILE=other.env ./scripts/run.sh   to use another settings file.
set -euo pipefail
cd "$(dirname "$0")/.."
ENV_FILE=${ENV_FILE:-.env}

load_env() {
  [[ -f "$ENV_FILE" ]] || { echo "missing $ENV_FILE: run './scripts/run.sh env' first"; exit 1; }
  local placeholder='^[^#]*(=CHANGE_ME$|DB_USER:|:DB_PASSWORD@)'
  if grep -qE "$placeholder" "$ENV_FILE"; then
    echo "$ENV_FILE still has placeholders:"; grep -nE "$placeholder" "$ENV_FILE"; exit 1
  fi
  set -a
  # shellcheck disable=SC1090
  . "./$ENV_FILE"
  set +a
}

case "${1:-run}" in
  env)
    if [[ -f .env ]]; then echo ".env already exists (delete it to regenerate)"; exit 0; fi
    cp .env.example .env
    chmod 600 .env
    rand_b64() { head -c "$1" /dev/urandom | base64 | tr -d '\n'; }
    set_var() { # set_var NAME VALUE (values never contain '|')
      sed -i "s|^$1=CHANGE_ME\$|$1=$2|" .env
    }
    keys=$(go run ./cmd/paygate keygen)
    set_var AUTH_SIGNING_KEY "$(sed -n 's/^AUTH_SIGNING_KEY=//p' <<<"$keys")"
    set_var AUTH_PUBLIC_KEYS "$(sed -n 's/^AUTH_PUBLIC_KEYS=//p' <<<"$keys")"
    set_var API_KEY_PEPPER "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    set_var DATA_ENCRYPTION_KEY "$(rand_b64 32)"
    admin_pw=$(rand_b64 18 | tr '+/' '-_')
    set_var BOOTSTRAP_ADMIN_PASSWORD "$admin_pw"
    echo "created .env with freshly generated secrets (file mode 600)"
    echo "  first admin: $(sed -n 's/^BOOTSTRAP_ADMIN_EMAIL=//p' .env)  password: $admin_pw"
    echo "  now set DATABASE_URL in .env (your Postgres user/password)"
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
  demo)
    load_env
    API=${API:-http://localhost${HTTP_ADDR}} ADMIN_EMAIL=$BOOTSTRAP_ADMIN_EMAIL ADMIN_PASSWORD=$BOOTSTRAP_ADMIN_PASSWORD \
      exec ./scripts/demo.sh
    ;;
  *)
    echo "usage: $0 [env|db|run|demo]"; exit 2
    ;;
esac
