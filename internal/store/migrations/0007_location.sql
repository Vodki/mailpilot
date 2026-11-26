-- Phase 6: mailbox actions. A message moved or deleted outside mailpilot
-- (another client, a server rule) is flagged absent so the lists stop
-- showing it; the watcher clears the flag if it comes back.
ALTER TABLE messages ADD COLUMN absent INTEGER NOT NULL DEFAULT 0;
