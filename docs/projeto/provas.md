# Provas, não features

[← índice](../README.md)

Quatro mecanismos transformam o projeto de demonstração em evidência. Dois deles rodam em segundos e provam a *engine*; dois rodam sob carga e provam o *sistema*.

---

## 1. Suíte de conformidade

Escrita **contra a interface `BidEngine` e nunca contra SQL** — é o que permite que a mesma suíte valide a engine em memória do shard e as duas baseadas em banco. Postgres real via `testcontainers`, um contêiner por pacote.

```go
// internal/bid/enginetest/conformance.go
func RunConformance(t *testing.T, newEngine func(*pgxpool.Pool) bid.BidEngine) {
    t.Run("alta_contencao", func(t *testing.T) {
        // 200 goroutines, 1 leilão, apostador agressivo.
        // Asserções válidas para as três engines:
        //   - nº de Accepted == nº de linhas em bids
        //   - seq sem buraco e sem repetição
        //   - amount estritamente crescente por seq
        //   - auctions.highest_bid_cents == max(bids.amount_cents)
        //   - nenhum Accepted devolveu Current desatualizado
    })
    t.Run("lance_baixo", ...)       // TooLow, com Current preenchido
    t.Run("leilao_fechado", ...)    // ends_at no passado → Closed
    t.Run("leilao_inexistente", ...)// NotFound
    t.Run("durabilidade", ...)      // todo Accepted está no banco ao retornar
}

// optimistic_test.go  → RunConformance(t, bid.NewOptimistic)
// pessimistic_test.go → RunConformance(t, bid.NewPessimistic)   // etapa 2
// shard_test.go       → RunConformance(t, bid.NewShard)         // etapa 3
```

Escrita agora, ela faz as etapas 2 e 3 custarem **duas linhas de teste cada**: a engine nova ou passa na suíte que já existe, ou está errada. Escrita depois, cada engine ganharia o seu teste sob medida e o projeto perderia a garantia de que as três respeitam o mesmo contrato — que é a premissa de compará-las.

---

## 2. Verificador de invariantes

`cmd/checker` roda contra o Postgres depois de cada célula do benchmark e valida propriedades que precisam valer independentemente da estratégia. Sai com código diferente de zero e **barra a matriz**.

### Invariantes provados só com SQL

```sql
-- sequência sem buraco e sem repetição (o UNIQUE já impede repetição)
SELECT auction_id FROM bids
 GROUP BY auction_id
HAVING count(*) <> max(seq) - min(seq) + 1;

-- valor estritamente crescente na ordem da sequência
SELECT auction_id FROM (
    SELECT auction_id, amount_cents,
           lag(amount_cents) OVER (PARTITION BY auction_id ORDER BY seq) AS prev
      FROM bids
) t WHERE prev IS NOT NULL AND amount_cents <= prev;

-- vencedor registrado é o maior valor aceito
SELECT a.id FROM auctions a
  JOIN (SELECT auction_id, max(amount_cents) m FROM bids GROUP BY auction_id) b
    ON b.auction_id = a.id
 WHERE a.highest_bid_cents <> b.m;

-- nenhum lance depois do fechamento — vale desde a etapa 1, sem worker
SELECT b.id FROM bids b JOIN auctions a ON a.id = b.auction_id
 WHERE b.created_at > a.ends_at;

-- nenhuma chave de idempotência repetida (etapa 2)
SELECT idempotency_key FROM bids
 WHERE idempotency_key IS NOT NULL
 GROUP BY idempotency_key HAVING count(*) > 1;
```

### O invariante que SQL sozinho não prova

*"Todo lance que recebeu `201` está no banco"* depende de um número que só existe **do lado do cliente**. O k6 exporta esse número, e o checker o consome:

```js
// bench/bid-storm.js
const accepted = new Counter('bids_accepted');
const maxSeq   = new Gauge('max_seq_seen');   // por leilão, via tag

export function handleSummary(data) {
  return { 'results/summary.json': JSON.stringify(data) };
}
```

```go
// cmd/checker
switch {
case dbCount < client.Accepted:
    fail("LANCE CONFIRMADO SUMIU: db=%d cliente=%d", dbCount, client.Accepted)
case dbCount > client.Accepted:
    // sem idempotência (etapa 1) isto é legítimo: o 201 saiu mas a
    // resposta não chegou ao cliente. Reportado, não falha.
    warn("%d respostas não chegaram ao cliente", dbCount-client.Accepted)
}
if dbMaxSeq < client.MaxSeqSeen {
    fail("seq confirmado ao cliente não existe no banco: %d < %d", dbMaxSeq, client.MaxSeqSeen)
}
```

A watermark de `seq` é o detalhe barato que pega write perdido: se o cliente viu a posição 4187 confirmada e o banco não a tem, um lance durável desapareceu — e isso custa um gauge, não um registro por requisição.

Comparar contra a métrica Prometheus do `auctiond` não serviria: seria o servidor conferindo a si mesmo. Se o handler mentir, a métrica mente junto e o invariante passa verde.

É isso que separa "parece funcionar" de "está provado sob 500 clientes simultâneos".

---

## 3. Duplicatas injetadas

*Etapa 2.*

O gerador de carga reenvia **10% das requisições com a mesma `X-Idempotency-Key`**, algumas em paralelo com a original. O relatório mostra zero lances duplos.

A idempotência não é descrita, é demonstrada. O caso interessante é a duplicata que chega **enquanto** a original ainda está em voo: resolvido com `SET key state=in_flight NX PX 30000`, onde o segundo request aguarda ou recebe `409 Idempotency-In-Flight`. Abaixo disso, o índice único parcial em `bids.idempotency_key` é a rede de segurança.

---

## 4. Caos

*Etapa 4.*

`make chaos` derruba componentes durante a carga:

| Alvo | Falha injetada | O que precisa continuar valendo |
| --- | --- | --- |
| `closerd` | `docker kill` no meio do processamento | O leilão fecha exatamente uma vez, via `XAUTOCLAIM` sobre a pending list. Como a guarda de fechamento é `now() < ends_at`, matá-lo nunca deixa entrar lance atrasado — só atrasa a materialização |
| `auctiond` | Kill de um pod no modo shard | Os leilões daquele shard são reassumidos, sem lance confirmado perdido. Como o `201` só sai após o commit em lote, não existe janela de aceite não persistido |
| Redis | Pausa de 5s | Requisições falham de forma limpa, sem lance aceito fora de ordem |
| PostgreSQL | Pool saturado artificialmente | Backpressure em vez de timeout em cascata |

Em todos os cenários, o verificador de invariantes precisa continuar verde.
