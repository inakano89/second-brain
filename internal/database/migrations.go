package database

// migrations are applied in order; never edit an applied entry, append a new one.
var migrations = []string{
	// 1 — core schema
	`
CREATE TABLE nodes (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	uid         TEXT NOT NULL UNIQUE,
	type        TEXT NOT NULL CHECK (type IN ('note','task','person','event','insight','article','health')),
	title       TEXT NOT NULL,
	content     TEXT NOT NULL DEFAULT '',
	summary     TEXT NOT NULL DEFAULT '',
	tags        TEXT NOT NULL DEFAULT '',
	source      TEXT NOT NULL DEFAULT '',
	source_ref  TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL DEFAULT '',
	due_at      TEXT,
	meta        TEXT NOT NULL DEFAULT '{}',
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE INDEX idx_nodes_type_created ON nodes(type, created_at);
CREATE INDEX idx_nodes_updated ON nodes(updated_at);
CREATE INDEX idx_nodes_status ON nodes(type, status);
CREATE INDEX idx_nodes_title ON nodes(title COLLATE NOCASE);
CREATE UNIQUE INDEX idx_nodes_source_ref ON nodes(source, source_ref) WHERE source_ref <> '';

CREATE VIRTUAL TABLE nodes_fts USING fts5(
	title, content, summary, tags,
	content='nodes', content_rowid='id',
	tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER nodes_ai AFTER INSERT ON nodes BEGIN
	INSERT INTO nodes_fts(rowid, title, content, summary, tags) VALUES (new.id, new.title, new.content, new.summary, new.tags);
END;
CREATE TRIGGER nodes_ad AFTER DELETE ON nodes BEGIN
	INSERT INTO nodes_fts(nodes_fts, rowid, title, content, summary, tags) VALUES ('delete', old.id, old.title, old.content, old.summary, old.tags);
END;
CREATE TRIGGER nodes_au AFTER UPDATE OF title, content, summary, tags ON nodes BEGIN
	INSERT INTO nodes_fts(nodes_fts, rowid, title, content, summary, tags) VALUES ('delete', old.id, old.title, old.content, old.summary, old.tags);
	INSERT INTO nodes_fts(rowid, title, content, summary, tags) VALUES (new.id, new.title, new.content, new.summary, new.tags);
END;

CREATE TABLE edges (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id  INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	target_id  INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	relation   TEXT NOT NULL DEFAULT 'related',
	weight     REAL NOT NULL DEFAULT 1.0,
	created_at TEXT NOT NULL,
	UNIQUE(source_id, target_id, relation),
	CHECK (source_id <> target_id)
);
CREATE INDEX idx_edges_target ON edges(target_id);

CREATE TABLE embeddings (
	node_id    INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
	model      TEXT NOT NULL,
	dim        INTEGER NOT NULL,
	vector     BLOB NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX idx_embeddings_model ON embeddings(model);

CREATE TABLE metrics (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	date       TEXT NOT NULL,
	kind       TEXT NOT NULL,
	value      REAL NOT NULL,
	unit       TEXT NOT NULL DEFAULT '',
	source     TEXT NOT NULL DEFAULT '',
	meta       TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL,
	UNIQUE(date, kind, source)
);
CREATE INDEX idx_metrics_kind_date ON metrics(kind, date);

CREATE TABLE logs (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	ts        TEXT NOT NULL,
	level     TEXT NOT NULL,
	component TEXT NOT NULL DEFAULT '',
	message   TEXT NOT NULL,
	meta      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_logs_ts ON logs(ts);
CREATE INDEX idx_logs_level_ts ON logs(level, ts);

CREATE TABLE task_queue (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	kind         TEXT NOT NULL,
	payload      TEXT NOT NULL DEFAULT '{}',
	status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','done','failed')),
	attempts     INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL DEFAULT 8,
	last_error   TEXT NOT NULL DEFAULT '',
	dedupe_key   TEXT,
	run_after    TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX idx_queue_ready ON task_queue(status, run_after);
CREATE UNIQUE INDEX idx_queue_dedupe ON task_queue(dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('pending','running');

CREATE TABLE llm_usage (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	ts            TEXT NOT NULL,
	provider      TEXT NOT NULL,
	model         TEXT NOT NULL,
	purpose       TEXT NOT NULL DEFAULT '',
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd      REAL NOT NULL DEFAULT 0
);
CREATE INDEX idx_usage_ts ON llm_usage(ts);

CREATE TABLE kv (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE chat_messages (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	channel TEXT NOT NULL,
	role    TEXT NOT NULL,
	content TEXT NOT NULL,
	ts      TEXT NOT NULL
);
CREATE INDEX idx_chat_channel ON chat_messages(channel, id);

CREATE TABLE seen_items (
	ns      TEXT NOT NULL,
	key     TEXT NOT NULL,
	seen_at TEXT NOT NULL,
	PRIMARY KEY (ns, key)
);
`,
}
