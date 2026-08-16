# Contrato HTTP

[← índice](../README.md)

Gin. Nenhum handler sabe qual engine está atrás dele: `BID_STRATEGY` troca a implementação sem alterar uma linha de `internal/httpapi`. Isso não é elegância, é condição do experimento — se o handler ramificasse por estratégia, a comparação estaria medindo também o handler.

---

## Endpoints

| Método | Rota | Entra na etapa |
| --- | --- | --- |
| `POST` | `/auctions` | 1 |
| `GET` | `/auctions/:id` | 1 |
| `POST` | `/auctions/:id/bids` | 1 |
| `GET` | `/healthz`, `/readyz` | 1 |
| `GET` | `/metrics` | 1 |
| `GET` | `/ws` | 5b |

---

## Identidade

`X-User-Id: <uuid>` no header. Sem JWT por enquanto.

Autenticação seria um custo constante e idêntico nas três estratégias, ou seja, não move nenhuma curva do gráfico — e a regra de escopo é justamente essa. Quando entrar, entra como middleware antes do handler, e nada abaixo dele muda.

---

## Dar um lance

```http
POST /auctions/7f3a.../bids
X-User-Id: 4c1e...
X-Idempotency-Key: 9b2d...        # etapa 2
Content-Type: application/json

{ "amountCents": 918500, "expectedVersion": 4186 }
```

`expectedVersion` é obrigatório no modo otimista e ignorado pelas outras duas engines.

### Aceito

```http
HTTP/1.1 201 Created

{
  "bidId": "e51c...",
  "seq": 4187,
  "currentVersion": 4187,
  "currentHighestBid": 918500,
  "minNextBid": 918600
}
```

O `201` significa **durável** — nas três engines. Ver [estrategias.md](estrategias.md#single-writer).

### Rejeitado

Todas as rejeições carregam o mesmo envelope, com o estado atual do leilão:

```http
HTTP/1.1 409 Conflict

{
  "error": "version_conflict",
  "currentVersion": 4187,
  "currentHighestBid": 918400,
  "minNextBid": 918500,
  "retryable": true
}
```

| Outcome | Status | `error` | `retryable` | Quem produz |
| --- | --- | --- | --- | --- |
| `Accepted` | `201` | — | — | as três |
| `Conflict` | `409` | `version_conflict` | `true` | só a otimista |
| `TooLow` | `422` | `too_low` | `true` | as três |
| `Closed` | `410` | `auction_closed` | `false` | as três |
| `NotFound` | `404` | `auction_not_found` | `false` | as três |
| erro de infra | `503` | `unavailable` | `true` | as três |

---

## Por que o estado vai no corpo de toda resposta

Duas razões, e as duas são sobre a validade do benchmark.

**Retentativa em um round-trip.** Se o cliente precisasse de um `GET` para descobrir a versão antes de cada tentativa, o benchmark passaria a medir throughput de leitura misturado com escrita. Com o estado no corpo do `409`, a retentativa é um único `POST` alimentado pela resposta anterior.

**Carga equivalente nas três engines.** Pessimista e shard nunca retornam `409`: elas serializam, e o perdedor recebe `422`. Se `422` fosse terminal, um VU no modo shard mandaria uma requisição e desistiria, enquanto o mesmo VU no modo otimista mandaria dez retentativas — as três estratégias receberiam cargas diferentes e `aceitos/s` deixaria de comparar qualquer coisa.

`409` e `422` são o mesmo evento visto por mecanismos diferentes: *alguém me passou na frente*. O cliente trata os dois igual; a distinção existe para a **métrica**, não para a decisão do cliente.

---

## Criar e consultar

```http
POST /auctions
{ "title": "Item 1", "startingBidCents": 0, "minIncrementCents": 100, "endsAt": "..." }
→ 201 { "id": "7f3a...", "version": 0, "minNextBid": 100, ... }
```

```http
GET /auctions/7f3a...
→ 200 { "id": "...", "version": 4187, "currentHighestBid": 918500,
        "minNextBid": 918600, "status": "open", "endsAt": "..." }
```

O k6 não usa nenhum dos dois no caminho quente, e nem no `setup`: `cmd/seed` cria os leilões direto no banco e escreve `bench/auctions.json` com `id`, `version` e `minNextBid` de cada um. Com 1000 leilões numa célula de contenção baixa, mil `GET` de preparação seriam ruído puro dentro da medição.

`POST /auctions` existe para o uso interativo e para o painel; a preparação do experimento não passa pela API.
