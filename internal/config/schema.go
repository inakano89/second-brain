package config

// Field describes one .env variable for the setup wizard and the web editor.
type Field struct {
	Key     string
	Label   string
	Group   string
	Default string
	Help    string
	Secret  bool
	Hidden  bool
	Kind    string // text, number, bool, textarea, select
	Options []string
}

// Groups defines the display order of the editor sections.
var Groups = []string{
	"Geral", "Segurança", "LLM", "Telegram", "Google", "Zepp", "RSS", "Backup", "Automação", "Atualizações", "Agendamentos",
}

// Schema is the catalogue of known configuration keys.
var Schema = []Field{
	{Key: "SETUP_COMPLETED", Group: "Geral", Default: "false", Hidden: true},
	{Key: "BRAIN_NAME", Label: "Nome do cérebro", Group: "Geral", Default: "Second Brain"},
	{Key: "HTTP_HOST", Label: "Host HTTP", Group: "Geral", Default: "0.0.0.0"},
	{Key: "HTTP_PORT", Label: "Porta HTTP", Group: "Geral", Default: "8080", Kind: "number"},
	{Key: "PUBLIC_URL", Label: "URL pública", Group: "Geral", Help: "Usada no OAuth do Google e no bookmarklet. Ex: https://brain.exemplo.com"},
	{Key: "TIMEZONE", Label: "Timezone (IANA)", Group: "Geral", Default: "America/Sao_Paulo"},
	{Key: "DATA_DIR", Label: "Diretório de dados", Group: "Geral", Default: "./data", Help: "Requer reinício."},
	{Key: "INBOX_DIR", Label: "Pasta monitorada (inbox)", Group: "Geral", Default: "./inbox"},
	{Key: "WATCHER_ENABLED", Label: "Folder watcher ativo", Group: "Geral", Default: "true", Kind: "bool"},
	{Key: "WATCHER_ACTION", Label: "Após processar", Group: "Geral", Default: "archive", Kind: "select", Options: []string{"archive", "delete"}},
	{Key: "QUEUE_WORKERS", Label: "Workers da fila", Group: "Geral", Default: "3", Kind: "number"},
	{Key: "LOG_RETENTION_DAYS", Label: "Retenção de logs (dias)", Group: "Geral", Default: "90", Kind: "number"},
	{Key: "IMPORT_MAX_MB", Label: "Limite de importação (MB)", Group: "Geral", Default: "200", Kind: "number", Help: "Tamanho máximo por envio na página Importar e em /api/import."},

	{Key: "ADMIN_USER", Label: "Usuário", Group: "Segurança"},
	{Key: "ADMIN_PASSWORD_HASH", Group: "Segurança", Hidden: true, Secret: true},
	{Key: "SESSION_SECRET", Group: "Segurança", Hidden: true, Secret: true},
	{Key: "API_TOKEN", Label: "API token (clipper/webhooks)", Group: "Segurança", Secret: true},

	{Key: "OPENAI_API_KEY", Label: "OpenAI API key", Group: "LLM", Secret: true, Help: "Guia: Ajuda → OpenAI"},
	{Key: "OPENAI_BASE_URL", Label: "OpenAI base URL", Group: "LLM", Default: "https://api.openai.com/v1"},
	{Key: "OPENAI_TRANSCRIBE_MODEL", Label: "Modelo de transcrição OpenAI", Group: "LLM", Default: "whisper-1"},
	{Key: "OPENAI_EMBED_MODEL", Label: "Modelo de embeddings OpenAI", Group: "LLM", Default: "text-embedding-3-small"},
	{Key: "ANTHROPIC_API_KEY", Label: "Anthropic (Claude) API key", Group: "LLM", Secret: true, Help: "Guia: Ajuda → Claude"},
	{Key: "GEMINI_API_KEY", Label: "Google Gemini API key", Group: "LLM", Secret: true, Help: "Guia: Ajuda → Gemini"},
	{Key: "GEMINI_EMBED_MODEL", Label: "Modelo de embeddings Gemini", Group: "LLM", Default: "gemini-embedding-001"},
	{Key: "OLLAMA_BASE_URL", Label: "LLM local (OpenAI-compatible)", Group: "LLM", Help: "Ex: http://localhost:11434/v1"},
	{Key: "OLLAMA_EMBED_MODEL", Label: "Embeddings locais", Group: "LLM", Default: "nomic-embed-text"},
	{Key: "EMBEDDING_PROVIDER", Label: "Provedor de embeddings", Group: "LLM", Default: "auto", Kind: "select", Options: []string{"auto", "openai", "gemini", "ollama", "local"}},

	// Models, per-task routing and council — managed on the /models page.
	{Key: "LLM_MODELS", Label: "Catálogo de modelos", Group: "Modelos", Hidden: true, Help: "provedor:modelo separados por vírgula",
		Default: "anthropic:claude-fable-5-1,anthropic:claude-opus-5-5,anthropic:claude-sonnet-5-5,anthropic:claude-haiku-4-5-20251001,openai:gpt-6-astra,openai:gpt-6-sol,openai:gpt-6-luna,gemini:gemini-3.8-flash,gemini:gemini-3.1-pro-preview,ollama:llama3.1"},
	{Key: "DEFAULT_LLM_PROVIDER", Label: "Provedor padrão global", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "ANTHROPIC_MODEL", Label: "Modelo padrão Claude", Group: "Modelos", Hidden: true, Default: "claude-opus-5-5"},
	{Key: "OPENAI_MODEL", Label: "Modelo padrão GPT", Group: "Modelos", Hidden: true, Default: "gpt-6-astra"},
	{Key: "GEMINI_MODEL", Label: "Modelo padrão Gemini", Group: "Modelos", Hidden: true, Default: "gemini-3.1-pro-preview"},
	{Key: "OLLAMA_MODEL", Label: "Modelo padrão local", Group: "Modelos", Hidden: true, Default: "llama3.1"},
	{Key: "LLM_ROUTE_CHAT", Label: "Tarefa: chat web", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_TELEGRAM", Label: "Tarefa: Telegram", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_ENRICH", Label: "Tarefa: auto-tagging", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_VISION", Label: "Tarefa: visão/OCR", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_TRANSCRIPTION", Label: "Tarefa: transcrição", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_EVENTS", Label: "Tarefa: eventos", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_EMAIL", Label: "Tarefa: e-mails", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_RSS", Label: "Tarefa: RSS", Group: "Modelos", Hidden: true, Default: "auto"},
	{Key: "LLM_ROUTE_BRIEFING", Label: "Tarefa: briefing matinal", Group: "Modelos", Hidden: true, Default: "council"},
	{Key: "LLM_ROUTE_REVIEW", Label: "Tarefa: revisões", Group: "Modelos", Hidden: true, Default: "council"},
	{Key: "LLM_COUNCIL_MEMBERS", Label: "Conselho: membros", Group: "Modelos", Hidden: true, Default: "anthropic,openai,gemini", Help: "provedor ou provedor:modelo"},
	{Key: "LLM_COUNCIL_JUDGE", Label: "Conselho: moderador", Group: "Modelos", Hidden: true, Default: "anthropic"},
	{Key: "LLM_COUNCIL_ROUNDS", Label: "Conselho: rodadas de debate", Group: "Modelos", Hidden: true, Default: "1", Kind: "number"},

	{Key: "LLM_PRICING", Label: "Override de preços (JSON)", Group: "LLM", Kind: "textarea", Help: `{"modelo-prefixo":[entrada_usd_1M, saida_usd_1M]}`},
	{Key: "AUTOLINK_THRESHOLD", Label: "Limiar de similaridade p/ auto-link", Group: "LLM", Help: "Vazio = automático (0.72 remoto / 0.45 local)"},

	{Key: "TELEGRAM_BOT_TOKEN", Label: "Token do bot", Group: "Telegram", Secret: true},
	{Key: "ALLOWED_TELEGRAM_USER_IDS", Label: "IDs autorizados", Group: "Telegram", Help: "Separados por vírgula"},

	{Key: "GOOGLE_CLIENT_ID", Label: "OAuth Client ID", Group: "Google"},
	{Key: "GOOGLE_CLIENT_SECRET", Label: "OAuth Client Secret", Group: "Google", Secret: true},
	{Key: "GOOGLE_SERVICES", Label: "Serviços Google", Group: "Google", Default: "calendar,gmail,drafts,drive,contacts,tasks,youtube",
		Help: "calendar, gmail, drafts (rascunhos), drive, contacts, tasks, youtube. Mudou a lista? Reconecte o Google."},
	{Key: "GOOGLE_CALENDAR_ID", Label: "Agenda para novos eventos", Group: "Google", Default: "primary"},
	{Key: "GOOGLE_CALENDARS", Label: "Agendas lidas", Group: "Google", Default: "all", Help: "all = todas as agendas visíveis no Google Agenda; ou IDs separados por vírgula"},
	{Key: "GOOGLE_CALENDAR_PAST_DAYS", Label: "Importar eventos passados (dias)", Group: "Google", Default: "365", Kind: "number", Help: "Importação única do histórico da agenda; 0 desliga"},
	{Key: "GMAIL_QUERY", Label: "Query de e-mails prioritários", Group: "Google", Default: "is:unread is:important newer_than:2d"},
	{Key: "GMAIL_NEWSLETTER_QUERY", Label: "Query de newsletters", Group: "Google", Help: "Ex: label:newsletters newer_than:1d"},
	{Key: "DRIVE_SINCE_DAYS", Label: "Drive: importar arquivos alterados nos últimos N dias", Group: "Google", Default: "365", Kind: "number", Help: "0 = todos. Vale para a primeira sincronização"},
	{Key: "DRIVE_MAX_FILES", Label: "Drive: máx. arquivos por ciclo", Group: "Google", Default: "40", Kind: "number"},
	{Key: "DRIVE_QUERY", Label: "Drive: filtro extra", Group: "Google", Help: "Sintaxe de busca do Drive. Ex: 'me' in owners"},

	{Key: "ZEPP_EMAIL", Label: "E-mail Zepp/Amazfit", Group: "Zepp"},
	{Key: "ZEPP_PASSWORD", Label: "Senha Zepp", Group: "Zepp", Secret: true},
	{Key: "ZEPP_APP_TOKEN", Label: "App token (alternativa ao login)", Group: "Zepp", Secret: true},
	{Key: "ZEPP_USER_ID", Label: "User ID", Group: "Zepp"},
	{Key: "ZEPP_API_BASE", Label: "API base", Group: "Zepp", Default: "https://api-mifit.huami.com"},

	{Key: "RSS_FEEDS", Label: "Feeds RSS/Atom", Group: "RSS", Kind: "textarea", Help: "Um por linha ou separados por vírgula"},
	{Key: "RSS_INTERESTS", Label: "Interesses (relevância)", Group: "RSS", Kind: "textarea"},
	{Key: "RSS_MIN_SCORE", Label: "Nota mínima (0-10)", Group: "RSS", Default: "6", Kind: "number"},
	{Key: "RSS_MAX_ITEMS", Label: "Máx. itens por ciclo", Group: "RSS", Default: "20", Kind: "number"},

	{Key: "BACKUP_ENCRYPTION_KEY", Label: "Chave mestre AES-GCM", Group: "Backup", Secret: true},
	{Key: "BACKUP_TARGETS", Label: "Destinos", Group: "Backup", Default: "local", Help: "local,s3,webdav,telegram"},
	{Key: "BACKUP_KEEP", Label: "Snapshots locais mantidos", Group: "Backup", Default: "7", Kind: "number"},
	{Key: "S3_ENDPOINT", Label: "S3 endpoint", Group: "Backup", Help: "Ex: https://s3.us-east-1.amazonaws.com"},
	{Key: "S3_REGION", Label: "S3 região", Group: "Backup", Default: "us-east-1"},
	{Key: "S3_BUCKET", Label: "S3 bucket", Group: "Backup"},
	{Key: "S3_ACCESS_KEY", Label: "S3 access key", Group: "Backup", Secret: true},
	{Key: "S3_SECRET_KEY", Label: "S3 secret key", Group: "Backup", Secret: true},
	{Key: "S3_PREFIX", Label: "S3 prefixo", Group: "Backup", Default: "second-brain/"},
	{Key: "S3_PATH_STYLE", Label: "S3 path-style", Group: "Backup", Default: "true", Kind: "bool"},
	{Key: "WEBDAV_URL", Label: "WebDAV URL (diretório)", Group: "Backup"},
	{Key: "WEBDAV_USER", Label: "WebDAV usuário", Group: "Backup"},
	{Key: "WEBDAV_PASSWORD", Label: "WebDAV senha", Group: "Backup", Secret: true},
	{Key: "BACKUP_TELEGRAM_CHAT_ID", Label: "Chat privado p/ backup", Group: "Backup"},

	{Key: "ACTIONS_ENABLED", Label: "Agent actions ativas", Group: "Automação", Default: "false", Kind: "bool"},
	{Key: "ACTIONS_FILE", Label: "Arquivo de ações", Group: "Automação", Default: "./actions.json"},
	{Key: "MQTT_BROKER", Label: "MQTT broker", Group: "Automação", Help: "tcp://host:1883 ou tls://host:8883"},
	{Key: "MQTT_USERNAME", Label: "MQTT usuário", Group: "Automação"},
	{Key: "MQTT_PASSWORD", Label: "MQTT senha", Group: "Automação", Secret: true},
	{Key: "MQTT_CLIENT_ID", Label: "MQTT client id", Group: "Automação", Default: "second-brain"},

	{Key: "AUTO_UPDATE_ENABLED", Label: "Instalar atualizações automaticamente", Group: "Atualizações", Default: "true", Kind: "bool", Help: "Baixa, verifica (SHA-256/assinatura), instala e reinicia a partir das releases do GitHub."},
	{Key: "UPDATE_CHANNEL", Label: "Canal", Group: "Atualizações", Default: "stable", Kind: "select", Options: []string{"stable", "prerelease"}},
	{Key: "UPDATE_REPO", Label: "Repositório GitHub", Group: "Atualizações", Default: "inakano89/second-brain", Help: "owner/repo de onde as releases são baixadas"},
	{Key: "LLM_MODELS_AUTO_SYNC", Label: "Atualizar lista de modelos de IA automaticamente", Group: "Atualizações", Default: "true", Kind: "bool", Help: "Todo dia baixa o catálogo de modelos do repositório: adiciona modelos novos, remove os descontinuados e segue o padrão recomendado (a menos que você tenha escolhido outro)."},

	{Key: "CRON_MORNING", Label: "Briefing matinal", Group: "Agendamentos", Default: "0 7 * * *"},
	{Key: "CRON_EVENING", Label: "Balanço noturno", Group: "Agendamentos", Default: "0 21 * * *"},
	{Key: "CRON_WEEKLY", Label: "Weekly review", Group: "Agendamentos", Default: "0 18 * * 0"},
	{Key: "CRON_MAINTENANCE", Label: "Manutenção", Group: "Agendamentos", Default: "30 3 * * *"},
	{Key: "CRON_BACKUP", Label: "Backup", Group: "Agendamentos", Default: "0 4 * * *"},
	{Key: "CRON_RSS", Label: "RSS", Group: "Agendamentos", Default: "*/30 * * * *"},
	{Key: "CRON_GMAIL", Label: "Gmail", Group: "Agendamentos", Default: "*/15 * * * *"},
	{Key: "CRON_CALENDAR", Label: "Calendar", Group: "Agendamentos", Default: "*/30 * * * *"},
	{Key: "CRON_DRIVE", Label: "Google Drive", Group: "Agendamentos", Default: "20 */2 * * *"},
	{Key: "CRON_CONTACTS", Label: "Contatos Google", Group: "Agendamentos", Default: "10 5 * * *"},
	{Key: "CRON_GOOGLE_TASKS", Label: "Google Tasks", Group: "Agendamentos", Default: "5,35 * * * *"},
	{Key: "CRON_YOUTUBE", Label: "YouTube", Group: "Agendamentos", Default: "25 5 * * *"},
	{Key: "CRON_TAKEOUT", Label: "Google Takeout no Drive", Group: "Agendamentos", Default: "45 5 * * *"},
	{Key: "CRON_ZEPP", Label: "Zepp", Group: "Agendamentos", Default: "0 */4 * * *"},
	{Key: "CRON_UPDATE", Label: "Verificar atualizações", Group: "Agendamentos", Default: "40 4 * * *"},
	{Key: "CRON_MODELS", Label: "Atualizar catálogo de modelos", Group: "Agendamentos", Default: "50 4 * * *"},
}

var schemaIndex = func() map[string]Field {
	m := make(map[string]Field, len(Schema))
	for _, f := range Schema {
		m[f.Key] = f
	}
	return m
}()

// Lookup returns the schema entry for key.
func Lookup(key string) (Field, bool) {
	f, ok := schemaIndex[key]
	return f, ok
}
