CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  display_name  TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('admin', 'member')),
  quota_bytes   INTEGER NOT NULL DEFAULT 0,
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);

CREATE TABLE devices (
  id           TEXT PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  platform     TEXT NOT NULL DEFAULT '',
  refresh_hash TEXT NOT NULL,
  refresh_exp  INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  revoked_at   INTEGER
);
CREATE INDEX devices_user ON devices(user_id);

CREATE TABLE pairing_tokens (
  token_hash TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER
);

CREATE TABLE spaces (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
  created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE space_members (
  space_id   INTEGER NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  permission TEXT NOT NULL CHECK (permission IN ('read', 'write')),
  PRIMARY KEY (space_id, user_id)
);

-- One row per file or directory. Replaced versions and trashed entries keep
-- their own rows (state = 'trashed') until purged.
CREATE TABLE files (
  id           INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused: clients track files by id
  owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('user', 'space')),
  owner_id     INTEGER NOT NULL,
  path         TEXT NOT NULL,
  parent       TEXT NOT NULL,
  name         TEXT NOT NULL,
  is_dir       INTEGER NOT NULL DEFAULT 0,
  size         INTEGER NOT NULL DEFAULT 0,
  mtime        INTEGER NOT NULL,          -- unix milliseconds
  sha256       TEXT,                      -- NULL for directories
  mime         TEXT NOT NULL DEFAULT '',
  version      INTEGER NOT NULL DEFAULT 1,
  seq          INTEGER NOT NULL DEFAULT 0, -- last change sequence touching this row
  state        TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'trashed')),
  trash_path   TEXT,                      -- set on the root of a trashed tree
  trash_root   INTEGER,                   -- id of the trashed root (self for the root)
  trash_reason TEXT,                      -- 'deleted' | 'replaced'
  trashed_at   INTEGER,
  device_id    TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX files_active_path ON files(owner_kind, owner_id, path) WHERE state = 'active';
CREATE INDEX files_active_parent ON files(owner_kind, owner_id, parent, name) WHERE state = 'active';
CREATE INDEX files_sha ON files(sha256, size) WHERE state = 'active';
CREATE INDEX files_trash ON files(state, trash_root);

CREATE TABLE changes (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_kind TEXT NOT NULL,
  owner_id   INTEGER NOT NULL,
  file_id    INTEGER NOT NULL,
  op         TEXT NOT NULL,   -- create | update | move | trash | restore | delete
  path       TEXT NOT NULL,
  old_path   TEXT,
  at         INTEGER NOT NULL
);
CREATE INDEX changes_owner ON changes(owner_kind, owner_id, seq);

CREATE TABLE upload_sessions (
  id           TEXT PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id    TEXT,
  owner_kind   TEXT NOT NULL,
  owner_id     INTEGER NOT NULL,
  path         TEXT NOT NULL,
  size         INTEGER NOT NULL,
  sha256       TEXT NOT NULL,
  mtime        INTEGER NOT NULL,
  base_version INTEGER,
  part_size    INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL
);

CREATE TABLE jobs (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL,
  key        TEXT NOT NULL,
  payload    TEXT NOT NULL DEFAULT '{}',
  state      TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'done', 'failed')),
  attempts   INTEGER NOT NULL DEFAULT 0,
  run_after  INTEGER NOT NULL,
  last_error TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (kind, key)
);
CREATE INDEX jobs_ready ON jobs(state, run_after);

-- Processor bookkeeping, keyed by content so duplicate files are processed once.
CREATE TABLE processor_state (
  processor  TEXT NOT NULL,
  sha256     TEXT NOT NULL,
  version    INTEGER NOT NULL,
  status     TEXT NOT NULL,   -- done | unsupported | failed
  error      TEXT,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (processor, sha256)
);

CREATE TABLE media_info (
  sha256       TEXT PRIMARY KEY,
  taken_at     INTEGER,
  width        INTEGER,
  height       INTEGER,
  orientation  INTEGER,
  camera_make  TEXT,
  camera_model TEXT,
  latitude     REAL,
  longitude    REAL
);
