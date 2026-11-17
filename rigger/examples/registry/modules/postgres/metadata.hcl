module {
  name        = "postgres"
  version     = "0.3.0"
  description = "A single PostgreSQL server: data on a host volume, application databases created by a hook, credentials from the secret store, optional periodic dumps to an S3 store."
  url         = "https://www.postgresql.org"
  license     = "PostgreSQL" # the official image is referenced, not shipped
  category    = "database"
  keywords    = ["postgres", "postgresql", "sql", "database"]

  requires {
    norca    = ">= 0.3.0"
    features = ["releases", "secret-store"]

    # Only when dumps are on: a release of this module must be in the stack.
    module "s3" {
      source  = "example/rustfs"
      version = ">= 0.2.0"
      when    = "backup.enabled"
    }
  }
}

ui {
  tab "general" { label = "General" }
  tab "databases" { label = "Databases" }
  tab "backup" { label = "Backups" }
}
