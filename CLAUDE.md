# CLAUDE.md

Guia para sessões do Claude Code neste repositório.

## Projeto

**Second Brain**: servidor Go auto-hospedado, binário único, open source (MIT), repositório **público** em
`github.com/inakano89/second-brain`. A interface, as mensagens ao usuário, a documentação e o guia `/help` ficam em **português (pt-BR)**.
Identificadores e comentários de código ficam em inglês.

## Ambiente e validação

- Go **1.27.1** na máquina do mantenedor; mínimo no `go.mod`: 1.26 (exigido por `modernc.org/sqlite` e `golang.org/x/*`).
- Sempre `CGO_ENABLED=0`. Alvos: linux/amd64, linux/arm64, linux/arm/v7, windows/amd64.
- **Não gerar binários para entrega**. Para validar, rode `make check`: faz `go vet`, roda os testes e compila todos os alvos com `-o /dev/null`. Nunca commitar `dist/`.
- Race detector: `CGO_ENABLED=1 go test -race ./...`.
- Antes de commitar, rode `gofmt -w ./cmd ./internal`.

## Mapa do código

| Pacote | Responsabilidade |
|---|---|
| `cmd/server` | flags, supervisor de serviços, rebind de porta, restart pós-update |
| `internal/config` | `.env` (lock + escrita atômica); **schema de todas as variáveis** em `schema.go` |
| `internal/database` | SQLite WAL, migrações, FTS5, grafo, vetores, fila, logs |
| `internal/llm` | provedores (OpenAI/Claude/Gemini/Ollama), roteamento por tarefa (`routes.go`), Conselho (`council.go`), catálogo curado + sync (`models.json`, `catalog*.go`), preços |
| `internal/agent` | ingestão, enriquecimento, busca híbrida, chat RAG + tools, ações |
| `internal/telegram`, `internal/integrations/*`, `internal/watcher` | canais de captura |
| `internal/scheduler` | cron, rotinas, backup |
| `internal/updater` | auto-update via GitHub Releases (+ modo overlay em container, rollback) |
| `internal/web` | handlers, templates HTMX (`templates/`), assets (`static/`) |

## Convenções

- **Nova variável de config** → `internal/config/schema.go` (aparece no editor web; use `Hidden: true` se for gerida por outra página, como `/models`). Depois regenere o `.env.example` com `go test ./internal/config -run EnvExample -update-env-example` (o teste falha se ele ficar desatualizado).
- **Migrações SQLite**: só acrescentar no fim de `internal/database/migrations.go`. Nunca editar uma migração já publicada.
- **Chamadas externas lentas ou falíveis** → fila offline (`internal/queue`). Marque erros definitivos com `queue.Permanent`.
- **LLM**: sempre via `llm.Manager`. O `Purpose` da requisição define a rota. Para criar uma tarefa roteável, adicione-a em `llm.Tasks` e crie a chave `LLM_ROUTE_*` no schema.
- **Web**: `html/template` + HTMX. A CSP proíbe scripts inline e `hx-on`, então JS novo vai em `internal/web/static/*.js`.
- **Concorrência**: use goroutines (`errgroup`/`WaitGroup`) para I/O paralelo. Estado compartilhado sempre com mutex e testado com `-race`.
- **Mudou um fluxo de configuração?** Atualize o `README.md` (usuários técnicos) e `internal/web/templates/help.html` (usuários leigos).
- **Assets de release** se chamam `second-brain-<os>-<arch>[.exe]`, junto com `SHA256SUMS` (e `.sig`). O auto-update depende desses nomes.

## Catálogo de modelos de IA (manter sempre atualizado)

A lista recomendada de modelos fica em `internal/llm/models.json`. Cada instalação baixa esse arquivo do `main` uma vez por dia e o aplica sozinha, sem precisar de release: modelos novos entram, os que saíram da lista são removidos e o ★ padrão acompanha a recomendação.

Fontes oficiais, que devem ser consultadas a cada atualização:
- Claude: https://platform.claude.com/docs/pt-BR/models/overview
- GPT: https://developers.openai.com/api/docs/models
- Gemini: https://ai.google.dev/gemini-api/docs/models

Para atualizar:
1. Edite `models.json`: modelos, o `default` de cada provedor (escolhido pelo mantenedor) e o `price` `[entrada, saída]` em US$ por 1M tokens. Se o preço não for conhecido, deixe o campo de fora; nunca invente um. Mudança de preço anunciada: `"price_changes": [{"from": "AAAA-MM-DD", "price": [in, out]}]`. Preço maior para prompts longos (ex.: Gemini acima de 200k): `"long_context": {"above": 200000, "price": [in, out]}`. Quando um modelo substitui outro, declare `"replaces": ["id-antigo"]`: as instalações movem para ele o padrão, as tarefas e o Conselho que usavam o antigo.
2. **Aumente `revision`** e atualize `updated`. Sem aumentar a revisão, nenhuma instalação aplica a mudança.
3. Espelhe a lista nos defaults de `LLM_MODELS` e `*_MODEL` em `internal/config/schema.go`; um teste confere que batem.
4. Regenere o `.env.example`, rode `make check` e abra o PR.

O workflow **Models watch** (`.github/workflows/models-watch.yml`, toda segunda-feira) lê as três páginas com `cmd/modelswatch` e abre ou atualiza a issue `models-watch` quando aparece um modelo que ainda não está no catálogo. Para silenciar um ID, coloque-o em `watch_ignore`.

## Repositório público — segurança

- Nunca commitar `.env`, chaves de API, tokens, `*.db`, backups ou `actions.json` (o `.gitignore` já cobre esses arquivos). Confira com `git status` antes do commit.
- Segredos só entram por `.env` ou pela UI. Em exemplos, use valores fictícios.
- Vulnerabilidades devem ser relatadas pelo processo do `SECURITY.md`, nunca em issues públicas.

## Git / GitHub

- **Exceção à preferência global "não uso git"**: este é o único projeto do mantenedor versionado com Git e publicado no GitHub (público). Aqui, **sempre** faça commit (mensagens claras, em inglês) e push da branch de trabalho ao concluir uma alteração.
- O mantenedor está começando com Git/GitHub. Quando uma ação envolver o GitHub, explique os comandos passo a passo.
- Fluxo de trabalho: branch de feature → Pull Request → merge em `main`. Release é uma tag `vX.Y.Z` em `main`, que dispara o workflow **Release**.

## Preferências do mantenedor

- Respostas concisas, em português.
- Use goroutines sempre que houver paralelismo útil.
- Teste se compila, mas não entregue binários compilados.
