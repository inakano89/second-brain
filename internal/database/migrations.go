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
	// 2 — trash (deleted nodes kept 30 days) and items the user deleted (not re-imported)
	`
CREATE TABLE trash (
	id         INTEGER PRIMARY KEY,
	type       TEXT NOT NULL,
	title      TEXT NOT NULL,
	source     TEXT NOT NULL DEFAULT '',
	node       TEXT NOT NULL,
	edges      TEXT NOT NULL DEFAULT '[]',
	batch      TEXT NOT NULL DEFAULT '',
	deleted_at TEXT NOT NULL
);
CREATE INDEX idx_trash_deleted ON trash(deleted_at);
CREATE INDEX idx_trash_batch ON trash(batch);

CREATE TABLE deleted_refs (
	source     TEXT NOT NULL,
	source_ref TEXT NOT NULL,
	deleted_at TEXT NOT NULL,
	PRIMARY KEY (source, source_ref)
);
`,
	// 3 — weekly cleanup suggestions (approved or dismissed on the Conteúdo page)
	`
CREATE TABLE cleanup_suggestions (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	kind        TEXT NOT NULL,
	action      TEXT NOT NULL,
	node_ids    TEXT NOT NULL,
	reason      TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL,
	status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','applied','dismissed')),
	created_at  TEXT NOT NULL,
	resolved_at TEXT
);
CREATE UNIQUE INDEX idx_cleanup_fingerprint ON cleanup_suggestions(fingerprint);
CREATE INDEX idx_cleanup_status ON cleanup_suggestions(status, kind);
`,
	// 4 — personal profile (identity, health, routine, dates, belongings) and check-ins
	`
CREATE TABLE profile_items (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL,
	title      TEXT NOT NULL,
	data       TEXT NOT NULL DEFAULT '',
	sensitive  INTEGER NOT NULL DEFAULT 0,
	archived   INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX idx_profile_kind ON profile_items(kind, archived);

CREATE TABLE profile_log (
	item_id INTEGER NOT NULL REFERENCES profile_items(id) ON DELETE CASCADE,
	day     TEXT NOT NULL,
	slot    TEXT NOT NULL DEFAULT '',
	at      TEXT NOT NULL,
	PRIMARY KEY (item_id, day, slot)
);
CREATE INDEX idx_profile_log_day ON profile_log(day);
`,
	// 5 — chat turns that used the personal profile: encrypted and kept out of the memory routine
	`ALTER TABLE chat_messages ADD COLUMN private INTEGER NOT NULL DEFAULT 0;`,
	// 6 — imported items that came without a date got the import moment as created_at: flag them so
	// searches, reports and listings do not mistake them for new content
	`
UPDATE nodes SET meta = json_set(meta, '$.date_unknown', json('true'))
WHERE source LIKE 'import:%' AND json_valid(meta)
	AND json_extract(meta, '$.import_at') IS NOT NULL
	AND julianday(created_at) >= julianday(json_extract(meta, '$.import_at')) - 0.0001
	AND julianday(created_at) <= julianday(json_extract(meta, '$.import_at')) + 0.25;
`,
	// 7 — spaced repetition: highlights and insights that come back at growing intervals
	`
CREATE TABLE reviews (
	node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
	step    INTEGER NOT NULL DEFAULT 0,
	cursor  INTEGER NOT NULL DEFAULT 0,
	next_at TEXT NOT NULL,
	last_at TEXT
);
CREATE INDEX idx_reviews_next ON reviews(next_at);
`,
	// 8 — bank statement lines (OFX / CSV): amounts in reais, negative = money out
	`
CREATE TABLE transactions (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	date        TEXT NOT NULL,
	amount      REAL NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	merchant    TEXT NOT NULL DEFAULT '',
	category    TEXT NOT NULL DEFAULT '',
	account     TEXT NOT NULL DEFAULT '',
	ref         TEXT NOT NULL,
	batch       TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	UNIQUE(account, ref)
);
CREATE INDEX idx_tx_date ON transactions(date);
CREATE INDEX idx_tx_merchant ON transactions(merchant, date);
`,
	// 9 — chat tabs: each web conversation is a row in chats and its turns live in channel
	// "web:<id>"; persona is a preset key ("" = general assistant, "custom" = instructions only).
	// The single history the web chat had until now becomes chat 1.
	`
CREATE TABLE chats (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	title        TEXT NOT NULL DEFAULT '',
	persona      TEXT NOT NULL DEFAULT '',
	instructions TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
INSERT INTO chats (id, title, created_at, updated_at)
SELECT 1, 'Conversa anterior', MIN(ts), MAX(ts) FROM chat_messages WHERE channel = 'web' HAVING COUNT(*) > 0;
UPDATE chat_messages SET channel = 'web:1' WHERE channel = 'web';
`,
}
