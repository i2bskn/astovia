// Used by the astovia-migrate image (working directory /migrate).
env "sqlite" {
  url = "sqlite:///app/data/astovia.db"
  src = "file:///migrate/sqlite/schema.hcl"
  dev = "sqlite://file?mode=memory"
}

env "postgres" {
  url = getenv("ASTOVIA_MIGRATE_URL")
  src = "file:///migrate/postgres/schema.hcl"
  dev = getenv("ASTOVIA_MIGRATE_DEV_URL")
  // Manage only the public schema so that other schemas in the database
  // (for example Supabase's auth and storage) are never dropped.
  schemas = ["public"]
}
