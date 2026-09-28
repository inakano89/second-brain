# Contribuindo

Obrigado pelo interesse! Issues e pull requests são bem-vindos.

## Ambiente

- Go 1.26+ (sem CGO).
- `make check` — `go vet`, testes e compilação de todos os alvos (linux amd64/arm64/armv7, windows amd64).
- `make run` — sobe o servidor com `./.env` (o setup wizard cria o arquivo no primeiro acesso).
- `SB_DEBUG=1` habilita logs de debug.

## Diretrizes

- Mantenha o binário **sem CGO** e sem dependências de runtime externas (assets via `embed.FS`, JS sem build step).
- Toda chamada externa lenta/falível deve passar pela fila offline (`internal/queue`) ou tolerar ausência de rede.
- Novas variáveis de configuração: registre em `internal/config/schema.go` (aparecem automaticamente no editor web) e regenere o `.env.example`.
- Migrações do banco: **apenas adicione** entradas em `internal/database/migrations.go`; nunca edite uma já publicada.
- Escreva testes para regras novas; rode `gofmt -w ./cmd ./internal` antes do commit.
- Commits no estilo [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`…) facilitam as notas de release.

## Releases (mantenedores)

1. `git tag vX.Y.Z && git push origin vX.Y.Z`, ou pela web em **Releases → Draft a new release** com uma tag nova. Tags com hífen (`v1.2.0-rc1`) viram pré-release; as demais são marcadas como estáveis e *latest*, o que o instalador e o auto-update exigem.
2. O workflow **Release** testa, compila os binários, gera `SHA256SUMS`, assina (`SHA256SUMS.sig`, se `UPDATE_SIGNING_KEY` estiver configurada), publica a release e a imagem multi-arch no GHCR.
3. Instâncias com atualização automática instalam a nova versão no próximo `CRON_UPDATE`.

Chave de assinatura (uma vez): `go run ./cmd/signer keygen` → salve a pública como *variable* `UPDATE_PUBLIC_KEY` e a privada como *secret* `UPDATE_SIGNING_KEY` do repositório.
