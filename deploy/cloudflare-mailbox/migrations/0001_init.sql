CREATE TABLE IF NOT EXISTS mailboxes (
  address TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  recipient TEXT NOT NULL,
  sender TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  raw TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_recipient_created
  ON messages(recipient, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_mailboxes_expires ON mailboxes(expires_at);
CREATE INDEX IF NOT EXISTS idx_messages_expires ON messages(expires_at);
