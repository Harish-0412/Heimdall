-- Disposable local-development passwords. Production passwords are supplied
-- through the operator's managed secret store, with TLS-enforced DB access.
CREATE ROLE heimdall_app LOGIN PASSWORD 'local-app-only'
  NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
