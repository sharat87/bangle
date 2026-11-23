-- Pre-registered OAuth clients were removed in the commit before this one:
-- every upstream login uses dynamic client registration.
ALTER TABLE servers DROP COLUMN client_id;
ALTER TABLE servers DROP COLUMN client_secret;
