#!/bin/sh
# Runs once, when PostgreSQL initialises an empty data directory (mounted into
# /docker-entrypoint-initdb.d by compose.personal.yaml). Creates the two roles
# of the corporate data model; passwords come from the container environment and
# never appear on a command line.
#   aiwiki_owner  owns the database and runs migrations (NOSUPERUSER NOBYPASSRLS)
#   aiwiki_app    runtime role of the wiki (NOSUPERUSER NOBYPASSRLS, owns nothing)
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\getenv owner_pw AIWIKI_OWNER_PASSWORD
\getenv app_pw AIWIKI_APP_PASSWORD
CREATE ROLE aiwiki_owner LOGIN PASSWORD :'owner_pw' NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
CREATE ROLE aiwiki_app LOGIN PASSWORD :'app_pw' NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
ALTER DATABASE aiwiki OWNER TO aiwiki_owner;
REVOKE ALL ON DATABASE aiwiki FROM PUBLIC;
GRANT CONNECT ON DATABASE aiwiki TO aiwiki_app;
GRANT ALL ON SCHEMA public TO aiwiki_owner;
SQL
