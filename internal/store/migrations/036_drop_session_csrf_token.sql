-- sessions.csrf_token was written on every sign-in and selected on every
-- authenticated request, but nothing outside the store package ever read it:
-- the live CSRF implementation is an independent signed double-submit cookie
-- that never consults the session. The column cost a random token per sign-in
-- and a wider row read on the hottest query in the application.
--
-- No index or constraint references it, so it can be dropped outright.
ALTER TABLE sessions DROP COLUMN csrf_token;
