#!/bin/sh
# Applies db/*/schema.hcl with the Atlas container to SQLite and PostgreSQL
# databases made from the schema before authlume (db/testdata/upgrade), with
# rows in them, the way `make migrate` does. It fails when the plan drops
# anything but the replaced tables or rebuilds a table, when the apply fails,
# when a second apply still has changes, or when the store does not accept
# the result (TestSchemaUpgrade in internal/store). No application database
# is touched: the databases live in work/ and in the test PostgreSQL.
set -eu
cd "$(dirname "$0")/.."
compose="docker compose -f compose.test.yaml"
dir=work/schema-upgrade
pg='postgres://astovia:development-only@postgres:5432'
pg_target="$pg/astovia_upgrade?sslmode=disable&search_path=public"
pg_dev="$pg/astovia_upgrade_dev?sslmode=disable&search_path=public"
dropped='sessions refresh_tokens oauth_clients oauth_grants oauth_tokens mfa_used_challenges'

psql() {
  $compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U astovia -d astovia_test "$@"
}
recreate_databases() {
  psql -c 'DROP DATABASE IF EXISTS astovia_upgrade' -c 'DROP DATABASE IF EXISTS astovia_upgrade_dev'
  if [ "${1:-}" = create ]; then
    psql -c 'CREATE DATABASE astovia_upgrade' -c 'CREATE DATABASE astovia_upgrade_dev'
  fi
}
cleanup() {
  recreate_databases >/dev/null 2>&1 || true
  # The files were written by the containers; remove them there.
  $compose run --rm -T go rm -rf "$dir" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

go_test() {
  $compose run --rm -T \
    -e ASTOVIA_TEST_UPGRADE_STAGE="$1" \
    -e ASTOVIA_TEST_UPGRADE_SQLITE="/src/$dir/astovia.db" \
    -e ASTOVIA_TEST_UPGRADE_POSTGRES="$pg/astovia_upgrade?sslmode=disable" \
    go sh -c "mkdir -p $dir && go test -count=1 -run '^TestSchemaUpgrade\$' ./internal/store && chmod -R a+rwX $dir"
}

# apply URL HCL DEV checks the plan, applies it and checks that nothing is
# left to apply.
apply() {
  plan=$($compose run --rm -T atlas schema apply --url "$1" --to "$2" --dev-url "$3" --dry-run)
  printf '%s\n' "$plan" | grep -E '^ +-> ' > "$dir/plan.txt" || true
  if grep -Eiq 'CREATE TABLE [`"]?new_|DROP COLUMN|DROP INDEX|RENAME' "$dir/plan.txt"; then
    printf '%s\n' "$plan" >&2
    echo "$2: the plan rebuilds a table or drops a column or an index" >&2
    exit 1
  fi
  found=$(grep -Eio 'DROP TABLE [^;]*' "$dir/plan.txt" | sed -E 's/^DROP TABLE //I; s/[`"]//g; s/^public\.//' | sort -u | tr '\n' ' ')
  want=$(printf '%s\n' $dropped | sort | tr '\n' ' ')
  if [ "$found" != "$want" ]; then
    printf '%s\n' "$plan" >&2
    echo "$2: the plan drops [$found], want [$want]" >&2
    exit 1
  fi
  $compose run --rm -T atlas schema apply --url "$1" --to "$2" --dev-url "$3" --auto-approve >/dev/null
  again=$($compose run --rm -T atlas schema apply --url "$1" --to "$2" --dev-url "$3" --dry-run)
  case "$again" in
    *'Schema is synced, no changes to be made'*) ;;
    *) printf '%s\n' "$again" >&2; echo "$2: changes left after the apply" >&2; exit 1 ;;
  esac
}

$compose up -d --wait postgres
recreate_databases create
go_test setup
apply "sqlite://$dir/astovia.db" file://db/sqlite/schema.hcl 'sqlite://file?mode=memory'
apply "$pg_target" file://db/postgres/schema.hcl "$pg_dev"
go_test verify
echo 'The schema applies to the databases of the schema before authlume.'
