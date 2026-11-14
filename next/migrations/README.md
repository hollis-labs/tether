# Migrations

One goose migration set, embedded by `migrations.go` and registered with `database.UseMigrationFS`.
Empty at slice S0; slice S1 adds the squashed schema as `00001_schema.sql`. Do not carry per-version
`ALTER` ladders from the old tree.
