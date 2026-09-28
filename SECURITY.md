# Política de segurança

## Reportando vulnerabilidades

**Não abra issues públicas para falhas de segurança.** Use
[GitHub Security Advisories](https://github.com/inakano89/second-brain/security/advisories/new)
para um relato privado. Inclua versão, passos de reprodução e impacto.

Respondemos em até 7 dias e publicamos a correção como release — instâncias com
atualização automática ativada recebem o patch sem intervenção.

## Versões suportadas

Apenas a release mais recente recebe correções.

## Cadeia de atualização

Atualizações automáticas só são instaladas se o binário conferir com o `SHA256SUMS`
da release e, quando o binário foi compilado com `UPDATE_PUBLIC_KEY`, se `SHA256SUMS.sig`
for uma assinatura ed25519 válida. Antes da troca, o banco é copiado e o binário novo
é testado; falhas repetidas de inicialização revertem para a versão anterior.
