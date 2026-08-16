# Schema e migrations

[← índice](../README.md)

`golang-migrate`, aplicado por um serviço `migrate` do compose que roda até completar antes do `auctiond` subir. O `auctiond` verifica a versão do schema no boot e **falha rápido** se não for a esperada, em vez de servir requisições contra um banco de formato diferente e produzir um benchmark inexplicável.

Valores monetários são `bigint` em **centavos**. Nunca ponto flutuante, e nem `numeric`: a coluna entra em `WHERE` no caminho mais quente do sistema e em agregação no verificador.

---

## `001_init`

```sql
CREATE TYPE auction_status AS ENUM ('open', 'closed');

CREATE TABLE auctions (
    id                  uuid PRIMARY KEY,
    title               text NOT NULL,
    status              auction_status NOT NULL DEFAULT 'open',
    highest_bid_cents   bigint NOT NULL DEFAULT 0 CHECK (highest_bid_cents >= 0),
    highest_bidder      uuid,
    min_increment_cents bigint NOT NULL DEFAULT 100 CHECK (min_increment_cents > 0),
    version             bigint NOT NULL DEFAULT 0,
    ends_at             timestamptz NOT NULL,
    closed_at           timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE bids (
    id              uuid PRIMARY KEY,
    auction_id      uuid NOT NULL REFERENCES auctions(id) ON DELETE CASCADE,
    user_id         uuid NOT NULL,
    amount_cents    bigint NOT NULL CHECK (amount_cents > 0),
    seq             bigint NOT NULL,
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (auction_id, seq)
);

CREATE UNIQUE INDEX bids_idempotency_key_uq
    ON bids (idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE INDEX bids_auction_seq_idx ON bids (auction_id, seq DESC);
```

---

## O que o banco garante sozinho

O schema não é só armazenamento: três decisões aqui transformam invariantes que o verificador teria que *detectar* em violações que o banco torna **impossíveis**.

### `UNIQUE (auction_id, seq)` — ordem total por leilão

`seq` é a `version` resultante do `UPDATE`, ou seja, a posição do lance na história do leilão. Com a restrição de unicidade, dois lances aceitos não podem ocupar a mesma posição. Se qualquer engine tiver uma corrida que aceite dois lances no mesmo ponto, o `INSERT` explode em vez de gravar uma história inconsistente que alguém descobriria três semanas depois.

Ordenar por `created_at` não serviria: sob 1000 VUs, dois lances empatam no mesmo microssegundo com frequência, e o verificador perderia a prova de ordem total.

### `UNIQUE` parcial em `idempotency_key` — já na `001`

A idempotência só chega na etapa 2, mas a coluna nasce agora. Adicionar índice único numa tabela com milhões de linhas depois é caro e ruidoso, e o custo de carregar uma coluna `NULL` até lá é zero. O índice parcial (`WHERE ... IS NOT NULL`) mantém fora dele todas as linhas da etapa 1.

Redis é a primeira linha de defesa contra duplicata; esta restrição é a rede embaixo — se o Redis for reiniciado ou uma chave expirar cedo, o banco ainda recusa o segundo lance.

### `ends_at` — fechamento é propriedade do tempo, não evento

A guarda de fechamento vive **na própria query**:

```sql
WHERE ... AND status = 'open' AND now() < ends_at
```

Isso significa que o invariante *"o leilão não recebeu lance depois do fechamento"* vale desde a etapa 1, sem existir worker nenhum. A coluna `status` entra no schema agora, mas só começa a ser escrita na etapa 4, quando o `closerd` materializa o vencedor e carimba `closed_at`.

A consequência é a que interessa: o `closerd` deixa de ser responsável pela **corretude** e passa a ser responsável só por **performance**. Matá-lo no teste de caos não pode fazer um lance atrasado entrar — no máximo atrasa a materialização.

---

## Sequência de valores

`version` começa em `0` e o primeiro lance aceito produz `seq = 1`. O incremento mínimo é do leilão, não do cliente:

```
minNextBid = highest_bid_cents + min_increment_cents
```

Se cada cliente escolhesse o próprio incremento, dois VUs competiriam sob regras diferentes e a comparação entre estratégias herdaria essa assimetria.

---

## Reset entre células do benchmark

`make bench` roda 36 células na mesma instância de Postgres. Sem reset, a célula 36 escreveria numa tabela com milhões de linhas e estatísticas velhas enquanto a célula 1 escreveu numa tabela vazia — e a última estratégia da lista pareceria a pior por efeito de ordem, não por mecanismo.

```sql
TRUNCATE bids, auctions RESTART IDENTITY CASCADE;
-- cmd/seed -auctions=$N
VACUUM ANALYZE;
```

Detalhado em [benchmark.md](benchmark.md#isolamento-entre-células).
