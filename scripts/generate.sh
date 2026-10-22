#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# This must be a disposable database: Atlas normalizes the HCL there.
# Never pass an application's database as ASTOVIA_SCHEMA_DEV_URL.
postgres_dev=${ASTOVIA_SCHEMA_DEV_URL:-docker://postgres/17/dev?search_path=public}
# Always inspect one schema so generated SQL is independent of dev URL style.
case "$postgres_dev" in
  *search_path=public|*search_path=public\&*) ;;
  *search_path=*) echo 'Schema generation requires search_path=public' >&2; exit 1 ;;
  *\?*) postgres_dev="$postgres_dev&search_path=public" ;;
  *) postgres_dev="$postgres_dev?search_path=public" ;;
esac
atlas schema inspect --url file://db/sqlite/schema.hcl --dev-url 'sqlite://file?mode=memory' --format '{{ sql . }}' > db/sqlite/schema.sql.tmp
# sqlc's SQLite parser requires standard double-quoted identifiers.
sed 's/`/"/g' db/sqlite/schema.sql.tmp > db/sqlite/schema.sql
atlas schema inspect --url file://db/postgres/schema.hcl --dev-url "$postgres_dev" --format '{{ sql . }}' > db/postgres/schema.sql.tmp
mv db/postgres/schema.sql.tmp db/postgres/schema.sql
# Keep schema.sql.tmp ignored; no application DB is altered here.
docker compose -f compose.test.yaml run --rm sqlc generate
