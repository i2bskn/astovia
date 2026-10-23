#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
compose="docker compose -f compose.test.yaml"
# Atlas runs in the compose.test.yaml "atlas" container, so the host needs no
# Atlas CLI. Commands whose stdout is captured use "run -T": a TTY would add
# CRLF line endings and mix stderr into the captured output.
atlas() {
  $compose run --rm -T atlas "$@"
}
# This must be a disposable database: Atlas normalizes the HCL there.
# Never pass an application's database as ASTOVIA_SCHEMA_DEV_URL.
# The URL is resolved inside the Atlas container, so it must be reachable from
# the compose.test.yaml network (for example a service name). docker:// URLs and
# localhost do not work there.
started_dev_db=false
cleanup() {
  if [ "$started_dev_db" = true ]; then
    $compose rm -sfv atlas-dev-postgres >/dev/null 2>&1 || true
  fi
  rm -f db/sqlite/schema.sql.tmp db/sqlite/schema.sql.tmp2 db/postgres/schema.sql.tmp
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [ -n "${ASTOVIA_SCHEMA_DEV_URL:-}" ]; then
  postgres_dev=$ASTOVIA_SCHEMA_DEV_URL
else
  postgres_dev='postgres://atlas:development-only@atlas-dev-postgres:5432/atlas_dev?sslmode=disable&search_path=public'
fi
# Always inspect one schema so generated SQL is independent of dev URL style.
case "$postgres_dev" in
  *search_path=public|*search_path=public\&*) ;;
  *search_path=*) echo 'Schema generation requires search_path=public' >&2; exit 1 ;;
  *\?*) postgres_dev="$postgres_dev&search_path=public" ;;
  *) postgres_dev="$postgres_dev?search_path=public" ;;
esac
atlas schema inspect --url file://db/sqlite/schema.hcl --dev-url 'sqlite://file?mode=memory' --format '{{ sql . }}' > db/sqlite/schema.sql.tmp
# sqlc's SQLite parser requires standard double-quoted identifiers. Only
# backquoted identifiers (letters, digits and underscores) are rewritten.
sed 's/`\([A-Za-z0-9_][A-Za-z0-9_]*\)`/"\1"/g' db/sqlite/schema.sql.tmp > db/sqlite/schema.sql.tmp2
mv db/sqlite/schema.sql.tmp2 db/sqlite/schema.sql
if [ -z "${ASTOVIA_SCHEMA_DEV_URL:-}" ]; then
  # Recreate the dev database on every run so leftovers cannot affect it.
  started_dev_db=true
  $compose up -d --wait --force-recreate atlas-dev-postgres
fi
atlas schema inspect --url file://db/postgres/schema.hcl --dev-url "$postgres_dev" --format '{{ sql . }}' > db/postgres/schema.sql.tmp
mv db/postgres/schema.sql.tmp db/postgres/schema.sql
# No application DB is altered here.
$compose run --rm sqlc generate
