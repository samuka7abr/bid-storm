# Etapa 1 — Spec 02: Contrato de lance e engine otimista

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [spec 01](01-spec-fundacao.md)

## Contexto

A spec 01 entregou a fundação: compose com recursos verificados, migration `001`, pool, `/healthz`, `/readyz` com fail-fast de schema, `/metrics` e `cmd/seed`. O comentário em `internal/httpapi/router.go` registra o que ficou de fora e para quando: *"Bid routes are deliberately absent: they arrive with the engine, in spec 02."*

Esta spec fecha o contrato HTTP inteiro da etapa 1 e entrega a primeira das três engines. É deliberadamente maior que a spec 01, e o motivo está na decisão 11: a suíte de conformidade escrita **agora**, contra a interface, é o único momento barato de descobrir que o contrato `BidEngine` está errado. Escrita depois, o erro aparece na etapa 3, com a API já publicada e duas engines para reescrever.

O que sustenta as decisões abaixo está em [decisoes/etapa-1.md](../../decisoes/etapa-1.md) — em especial as decisões **1** (classificação, não colapso), **2** (estado em toda resposta), **3** (`bids` só guarda aceitos), **9** (`409` e `422` com o mesmo envelope), **10** (`Outcome` no resultado, `error` só para infra), **11** (conformidade contra a interface), **12** (`ends_at` como guarda) e **17** (`min_increment_cents` é do leilão), mais as decisões **19** a **24**, tomadas nesta spec e que emendam quatro pontos de `estrategias.md` e `api.md`.

## Objetivo

Publicar o contrato que as três engines vão implementar, e provar esse contrato com uma engine real e uma suíte que roda em segundos.

O sistema deve:

- Expor `BidEngine` como um contrato de um método, onde rejeição de lance é resultado e `error` significa apenas *não consegui decidir*
- Implementar a engine otimista com o caminho feliz em **um único statement** e a classificação pagando round-trip só quando o lance falha
- Servir `POST /auctions`, `GET /auctions/:id` e `POST /auctions/:id/bids`, com um envelope que nunca publica número inventado
- Ligar `BID_STRATEGY` à seleção da engine, sem que nenhum handler saiba qual está atrás dele
- Medir as três engines futuras do mesmo ponto, por construção e não por disciplina
- Provar, sob concorrência real, que a história gravada de um leilão é reexecutável passo a passo

## Fora de Escopo

- Engine pessimista — etapa 2. Engine single-writer — etapa 3
- Middleware de idempotência, `X-Idempotency-Key` e uso do Redis — etapa 2. A coluna `idempotency_key` continua `NULL` em toda linha escrita aqui
- `cmd/checker` e o `handleSummary` do k6 — spec 03
- `bid_attempts_per_accept`, que na etapa 1 mora no k6 (decisão 16)
- `RETRY_POLICY`, `MAX_RETRIES` e `BID_DEADLINE`: são do apostador, e o apostador é do k6
- `closerd`, materialização de `status`, WebSocket, painel React
- Dashboards do Grafana — etapa 5

## Fluxo

```text
POST /auctions/:id/bids
  ├── middleware X-User-Id      ausente/inválido → 400 invalid_user_id
  ├── bind do corpo             amountCents ausente/<=0 → 400 invalid_amount
  └── app.Engine.PlaceBid(c.Request.Context(), req)
        └── instrumented (decorator)
              ├── t := now
              ├── optimistic.PlaceBid
              │     ├── ExpectedVersion == nil → Invalid          (0 statements)
              │     ├── CTE: UPDATE ... RETURNING + INSERT bids   (1 statement)
              │     │     ├── 1 linha  → Accepted, seq, bidID
              │     │     └── 0 linhas → classify
              │     └── classify: SELECT ..., now()               (+1 statement)
              │           ├── ErrNoRows                → NotFound
              │           ├── IsClosed(dbNow)          → Closed
              │           ├── Version != *Expected     → Conflict
              │           └── Amount < MinNextBid()    → TooLow
              ├── bid_confirm_duration_seconds{strategy}.Observe
              └── bid_outcomes_total{strategy,outcome}.Inc
        │
        └── handler: tabela de mapeamento
              Accepted → 201 BidAccepted
              Conflict → 409 BidRejected   version_conflict     retryable
              TooLow   → 422 BidRejected   too_low              retryable
              Closed   → 410 BidRejected   auction_closed
              NotFound → 404 ErrorResponse auction_not_found
              Invalid  → 400 ErrorResponse expected_version_required
              error    → 503 ErrorResponse unavailable          retryable
```

## Decisoes Tecnicas

### O caminho feliz é um statement, não uma transação

`estrategias.md` ilustra o mecanismo com `Begin` → `UPDATE ... RETURNING` → `INSERT` → `Commit`. Isso são quatro round-trips por tentativa, com a conexão presa nos quatro. Aqui o caminho feliz é uma CTE:

```sql
WITH upd AS (
    UPDATE auctions
       SET highest_bid_cents = $1,
           highest_bidder    = $2,
           version           = version + 1
     WHERE id      = $3
       AND version = $4
       AND status  = 'open'
       AND now() < ends_at
       AND highest_bid_cents + min_increment_cents <= $1
    RETURNING version, min_increment_cents
),
ins AS (
    INSERT INTO bids (id, auction_id, user_id, amount_cents, seq)
    SELECT $5, $3, $2, $1, upd.version FROM upd
    RETURNING seq
)
SELECT ins.seq, upd.min_increment_cents FROM ins, upd;
```

Um statement, atômico por construção, um round-trip. Se o `UPDATE` não casar, `upd` fica vazia, `ins` não insere nada e o `SELECT` final devolve zero linhas — `pgx.ErrNoRows`, que é o gatilho da classificação.

O `RETURNING` do `INSERT` só enxerga colunas da tabela alvo, e é por isso que `min_increment_cents` sai no `SELECT` final em vez de no `RETURNING`. Ele é necessário porque o `201` publica `minNextBid`, e sem ele o caminho feliz precisaria de uma segunda consulta só para montar o corpo.

Isso dá ao otimista uma vantagem que a pessimista não pode ter, já que `SELECT ... FOR UPDATE` exige transação aberta. A vantagem é legítima e fica registrada: **precisar de transação é um custo real do pessimismo**, e escondê-lo dando uma transação de graça ao otimista pesaria a balança a favor da hipótese do projeto. Os quatro invariantes de método fixam pool, CPU, memória e estado inicial — nenhum deles pede paridade de round-trips.

### A classificação usa um snapshot novo

Sem transação aberta, o `classify` custa uma segunda aquisição do pool. Ele continua valendo a pena, e continua sendo pago apenas no caminho de falha.

A alternativa de ler o estado dentro da mesma CTE foi descartada: todas as CTEs de um statement compartilham o snapshot do início dele, então o estado devolvido no `409` seria anterior ao commit do vencedor que acabou de passar. O apostador da decisão 5 re-mira em `minNextBid`, e re-mirar num valor velho produz uma retentativa que já nasce condenada — `bid_attempts_per_accept` subiria por artefato de implementação, não por contenção.

### `Conflict` tem precedência sobre `TooLow`

`estrategias.md` classifica na ordem `Closed` → `TooLow` → `Conflict`. Combinada com o apostador agressivo, essa ordem esvazia a métrica-chave do otimista: o VU re-mira em `minNextBid`, outro VU passa na frente, e a rejeição chega com versão velha **e** valor abaixo do novo mínimo — ou seja, quase toda rejeição sob alta contenção vira `TooLow`, e `bid_outcomes_total{outcome="conflict"}` vai a zero justamente na célula onde a tese precisa dela.

A ordem aqui é `NotFound` → `Closed` → `Conflict` → `TooLow`. `Conflict` passa a significar *o snapshot do cliente ficou velho*, e `TooLow` passa a significar *o cliente estava atualizado e ainda assim mandou pouco*.

Isso não reabre o caso que a decisão 1 protege. O lance de R$ 5 num leilão de R$ 9.000 não vira retentativa infinita porque o corpo da resposta carrega `minNextBid` e o apostador re-mira nele (decisão 5), e porque `MAX_RETRIES` e `BID_DEADLINE` limitam de qualquer forma (decisão 18). O que a decisão 1 impede é colapsar os quatro desfechos num só; a ordem entre eles é outra escolha, e é esta.

### `Outcome` cobre o que a engine decide, inclusive não poder decidir

`expectedVersion` é obrigatório no otimista e ignorado nas outras duas (`api.md`). Mas `api.md` também proíbe o handler de saber qual engine está atrás dele, e chama isso de condição do experimento — um `switch` por estratégia no handler faria a comparação medir também o handler.

A saída é `Outcome.Invalid`: a engine otimista devolve esse desfecho quando `ExpectedVersion` é `nil`, e o handler ganha mais uma linha na tabela de mapeamento. A interface continua com um método só, que é o que a suíte de conformidade exercita, e `error` continua reservado a falha de infraestrutura (decisão 10).

Tem um efeito colateral útil: `bid_outcomes_total{outcome="invalid"}` maior que zero durante o benchmark denuncia k6 mal configurado, de graça.

### O envelope não publica número inventado

`estrategias.md` afirma que `Current` é preenchido sempre, e a decisão 10 repete. Isso é falso para `NotFound` — o leilão não existe, não há estado — e para `Invalid`, em que a engine responde sem tocar no banco.

Então há três formas de corpo, e `AuctionStateView` é embutida só nas duas que têm estado real:

```go
type AuctionStateView struct {
    CurrentVersion    int64 `json:"currentVersion"`
    CurrentHighestBid int64 `json:"currentHighestBid"`
    MinNextBid        int64 `json:"minNextBid"`
}

type BidAccepted struct {
    AuctionStateView
    BidID uuid.UUID `json:"bidId"`
    Seq   int64     `json:"seq"`
}

type BidRejected struct {
    AuctionStateView
    Error     string `json:"error"`
    Retryable bool   `json:"retryable"`
}

type ErrorResponse struct {
    Error     string `json:"error"`
    Retryable bool   `json:"retryable"`
}
```

Com a view embutida, o compilador passa a garantir o que a decisão 2 exige: nenhum corpo de `201`, `409`, `422` ou `410` pode nascer sem o estado do leilão. E `404`, `400` e `503` deixam de anunciar que um leilão inexistente vale zero centavos.

`seq` e `currentVersion` carregam o mesmo número no `201`, porque `seq` **é** a `version` resultante (decisão 3). Os dois ficam, porque `api.md` publica os dois.

### Fechado é uma regra só, decidida pelo relógio do Postgres

A coluna `status` só passa a ser escrita na etapa 4. Até lá ela vale `'open'` para todo leilão, inclusive os vencidos — mas a engine recusa lance neles. Se `GET /auctions/:id` devolvesse a coluna crua, as duas rotas da mesma API discordariam sobre o mesmo leilão, e o painel da etapa 5b renderizaria leilão morto como vivo.

A regra vira um método, e o `classify` e o `GET` chamam o mesmo método:

```go
func (s AuctionState) IsClosed(now time.Time) bool {
    return s.Status == Closed || !now.Before(s.EndsAt)
}
```

O `now` **não** é `time.Now()`. O `UPDATE` decide o aceite com `now()` do Postgres; se a classificação decidisse com o relógio do container do `auctiond`, um skew de alguns milissegundos produziria `410 auction_closed` com `retryable: false` para um lance que o banco teria aceito — e o VU desistiria. Isso morde exatamente no último segundo do leilão, que é o cenário inteiro do projeto, e o erro seria enviesado, não aleatório.

Por isso o `SELECT` do `classify` e o do `GET` devolvem `now()` junto com as colunas. Custa zero round-trip, e faz aceite, classificação e leitura serem incapazes de discordar.

Quando o `closerd` chegar na etapa 4 e passar a escrever a coluna, `IsClosed` já cobre o caso e nenhum handler muda — que é precisamente o que a decisão 12 promete.

### As três engines são medidas do mesmo ponto, por construção

`bid_confirm_duration_seconds` e `bid_outcomes_total` são observadas por um decorator sobre `BidEngine`, não dentro de cada implementação:

```go
func Instrument(next bid.BidEngine, reg prometheus.Registerer, strategy string) bid.BidEngine
```

Se cada engine instrumentasse a si mesma, nada no compilador impediria o shard da etapa 3 de cronometrar de um ponto mais generoso que o otimista — que é exatamente o viés silencioso que os quatro invariantes de método existem para impedir, e a primeira coisa que um revisor sério procuraria. Com o decorator, as três são medidas na mesma fronteira porque só existe uma fronteira.

O shard vai precisar de `bid_accept_duration_seconds`, o instante da decisão em memória (decisão 8). Esse instante não existe fora dele, então ele instrumenta essa série por dentro, na etapa 3. `confirm` continua vindo do decorator nas três.

O label `strategy` entra nas duas séries. O processo só roda uma estratégia por vez, então não há custo de cardinalidade, e a série se auto-identifica quando os resultados das 36 células forem exportados.

### Layout de pacotes: o contrato não pode importar as engines

`internal/bid` guarda o contrato e a suíte; cada engine mora num subpacote; a factory mora em `internal/app`.

O motivo é mecânico. A suíte importa o contrato, e cada engine importa o contrato — então qualquer coisa que importe as três engines não pode morar no pacote do contrato, ou o ciclo é imediato. Pôr a factory em `internal/app` torna o ciclo impossível e mantém `internal/bid` sem as dependências que o shard vai trazer na etapa 3 (goroutines, ticker, batching): quem importar `bid` só para ler o tipo `Outcome` não arrasta nada disso junto.

`internal/app` é o único lugar do código que conhece os nomes das estratégias.

### A fila do pool é medida, não cortada

O handler passa `c.Request.Context()` inteiro até a engine, e não impõe timeout próprio.

O `net/http` cancela esse contexto quando o cliente desconecta, então o apostador desistindo por `BID_DEADLINE` (decisão 18) libera sozinho a vaga na fila do pool, e `db_pool_canceled_acquire_total` — série que a spec 01 já criou — conta isso.

Um timeout no servidor seria um quinto parâmetro a manter idêntico nas três engines, e cortaria a fila que é justamente o fenômeno sob medição: a decisão 7 diz que *o pool é a história do pessimista*. Um corte em 500ms mediria o timeout, não o mecanismo.

### A suíte prova a história inteira, não só a sequência

`enginetest.RunConformance(t, factory)` é escrita contra `BidEngine` e nunca contra SQL, para que a engine em memória do shard possa passar nela (decisão 11).

O teste de concorrência não se contenta com `seq` único e crescente: isso o `UNIQUE (auction_id, seq)` já torna impossível, e a suíte estaria confirmando o Postgres em vez da engine. Ela lê os lances em ordem de `seq` e **reexecuta a regra a cada passo**: a sequência tem que ser `1..A` sem buraco, cada `amount_cents` tem que ser maior ou igual ao anterior mais `min_increment_cents`, o total tem que bater com os `Accepted` contados pelo teste, e o estado reconstruído tem que bater com a linha de `auctions`.

Essa é a asserção que importa para a etapa 3. O otimista carrega `highest_bid_cents + min_increment_cents <= $1` dentro do `WHERE`, então o banco o impede de aceitar lance inválido. O shard decide em memória, sem `WHERE` nenhum — uma corrida ali aceitaria um lance abaixo do mínimo no meio da história, deixaria a sequência perfeita, e passaria despercebida por qualquer teste mais fraco.

### Observabilidade

Entram as duas séries que a decisão 16 aloca à etapa 1, e nada além:

| Série | Tipo | Labels |
| --- | --- | --- |
| `bid_outcomes_total` | Counter | `strategy`, `outcome` |
| `bid_confirm_duration_seconds` | Histogram | `strategy` |

`bid_conflicts_total` não existe: conflito é o label `outcome="conflict"` (decisão 16). `bid_attempts_per_accept` continua no k6 nesta etapa, porque sem chave de idempotência o servidor não tem como correlacionar três requisições como tentativas do mesmo lance.

Buckets do histograma cobrindo de 1ms a 8s, porque a hipótese prevê `p99` na casa das centenas de milissegundos no otimista sob alta contenção, e um bucket máximo de 1s truncaria justamente a cauda que o projeto quer mostrar.

## Requisitos Funcionais

### RF01 - Contrato `BidEngine`

`internal/bid/engine.go` define `BidEngine` com um método, mais `Outcome`, `BidRequest`, `AuctionState` e `BidResult` como em [projeto/estrategias.md](../../projeto/estrategias.md), com duas emendas: `Outcome` ganha `Invalid`, e `AuctionState` ganha `IsClosed(now time.Time) bool` junto de `MinNextBid() int64`.

`Outcome` implementa `String()` devolvendo `accepted|conflict|too_low|closed|not_found|invalid`, que é o valor do label da métrica.

O pacote não importa Prometheus, Gin nem nenhuma engine.

### RF02 - Engine otimista, caminho feliz

`internal/bid/optimistic` implementa `BidEngine` com o statement único descrito em Decisões Técnicas. Um round-trip, sem `Begin`/`Commit`, sem segunda consulta para montar o corpo do `201`.

`BidResult.Current` no aceite é montado a partir do próprio retorno: `Version` e `Seq` são a `version` devolvida, `HighestBidCents` é o valor recém-aceito, e `MinIncrementCents` vem do `SELECT` final.

### RF03 - Engine otimista, classificação

`ExpectedVersion == nil` devolve `Invalid` antes de qualquer statement.

`pgx.ErrNoRows` no statement principal dispara `classify`, que roda um `SELECT` separado devolvendo `version`, `highest_bid_cents`, `min_increment_cents`, `status`, `ends_at` e `now()`, e classifica na ordem `NotFound` → `Closed` → `Conflict` → `TooLow`, com `Conflict` como padrão.

`Current` é preenchido em `Closed`, `Conflict` e `TooLow`. Em `NotFound` e `Invalid` fica zerado e o handler não o publica.

### RF04 - Seleção da engine

`internal/app.NewEngine(strategy string, pool *pgxpool.Pool, reg prometheus.Registerer) (bid.BidEngine, error)` devolve a engine correspondente a `BID_STRATEGY`, já embrulhada no decorator de métricas.

`optimistic` é a única implementada. `pessimistic` e `shard` devolvem erro nomeando a etapa em que chegam, para que `BID_STRATEGY=shard` hoje falhe no boot com uma mensagem e não com um `nil` mais adiante. Valor desconhecido também falha no boot.

`cmd/auctiond` passa a chamar `app.NewEngine` e a abortar se ela falhar. O evento de boot continua logando `strategy`.

### RF05 - `POST /auctions/:id/bids`

Corpo `{ "amountCents": int64, "expectedVersion": *int64 }`, header `X-User-Id: <uuid>`.

Middleware valida `X-User-Id` e responde `400 invalid_user_id` se ausente ou não for UUID. Ele é montado **só** nas rotas de lance: não há dono de leilão no schema, então `POST /auctions` não pede identidade.

`:id` que não for UUID responde `404 auction_not_found`, sem consultar o banco. `amountCents` ausente ou não positivo responde `400 invalid_amount`, sem chamar a engine — a coluna tem `CHECK (amount_cents > 0)` e deixar o banco recusar transformaria erro de cliente em `503`.

O mapeamento de `Outcome` para status, corpo e `retryable` é exatamente a tabela do Fluxo. `error` não-nil vira `503 unavailable` com `retryable: true`, e é logado com o `auction_id`.

### RF06 - `POST /auctions` e `GET /auctions/:id`

`POST /auctions` aceita `{ "title", "startingBidCents", "minIncrementCents", "endsAt" }`, gera o `id` no servidor e devolve `201` com `id`, `title`, `status`, `endsAt` e `AuctionStateView`. Rejeita com `400` e `ErrorResponse`: `title` vazio, `endsAt` ausente ou não futuro, `minIncrementCents <= 0`, `startingBidCents < 0`.

`GET /auctions/:id` devolve `200` com `id`, `title`, `status`, `endsAt` e `AuctionStateView`, ou `404 auction_not_found`. `status` é derivado por `IsClosed` sobre o `now()` que a própria query devolve, nunca a coluna crua.

Nenhuma das duas rotas é usada pelo k6, nem no caminho quente nem no `setup` ([api.md](../../projeto/api.md)): existem para uso interativo e para o painel da etapa 5b.

### RF07 - Métricas de lance

`internal/metrics` ganha `Instrument(next bid.BidEngine, reg prometheus.Registerer, strategy string) bid.BidEngine`, que observa `bid_confirm_duration_seconds{strategy}` e incrementa `bid_outcomes_total{strategy,outcome}` a cada chamada, registrando no mesmo registry privado da spec 01.

Chamada que devolve `error` conta em `bid_outcomes_total` com `outcome="error"` e não entra no histograma: latência de infra caída não é latência de lance.

### RF08 - Suíte de conformidade

`internal/bid/enginetest.RunConformance(t *testing.T, newEngine func(*pgxpool.Pool) bid.BidEngine)` roda contra `internal/testsupport`, cobrindo:

- Primeiro lance aceito produz `seq = 1` e `version = 1`
- Lance abaixo de `MinNextBid()` devolve `TooLow` com `Current` preenchido
- Lance em leilão inexistente devolve `NotFound`
- Lance em leilão com `ends_at` no passado devolve `Closed`
- Rejeição não escreve linha em `bids` (decisão 3)
- `MinNextBid()` do corpo permite ao chamador acertar na tentativa seguinte
- Concorrência: N goroutines num leilão só, seguida do replay descrito em Decisões Técnicas

Casos que só uma engine produz — `Conflict` e `Invalid`, ambos dependentes de `ExpectedVersion` — ficam em `optimistic_test.go`, não na suíte: a pessimista e o shard ignoram `ExpectedVersion` por contrato e falhariam neles por design.

## Requisitos Nao Funcionais

- Nenhuma dependência de runtime nova: `pgx/v5`, `gin`, `prometheus/client_golang` e `google/uuid` continuam sendo tudo
- `internal/bid` não importa Prometheus, Gin nem `pgxpool`; `internal/httpapi` não importa nenhuma engine concreta
- `go vet ./...` limpo; `gofmt -l .` vazio
- A suíte de conformidade completa roda em menos de 30 segundos, contêiner incluído
- Nenhum `switch` por nome de estratégia fora de `internal/app`
- Log estruturado apenas em `503` e no boot: uma linha por requisição sob 1000 VUs é custo mensurável dentro da coisa medida
- Toda rejeição é `nil` no `error` da engine, sem exceção

## Budget do PR

Até 25 arquivos e aproximadamente 800 linhas de código próprio, sem contar `go.sum`.

Isso é maior que o budget da spec 01, e por escolha registrada: fechar o contrato HTTP da etapa 1 num PR só evita publicar um envelope pela metade, e a suíte de conformidade da decisão 11 só rende o que promete se nascer junto da primeira engine. Se passar de 25 arquivos ou 800 linhas, pare e reporte.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: medio
- Referencia permitida: `docs/projeto/estrategias.md`, `docs/projeto/api.md`, `docs/projeto/schema.md`, `docs/projeto/observabilidade.md`, `docs/decisoes/etapa-1.md`, `docs/specs/etapa-1/01-spec-fundacao.md`, `docs/specs/etapa-1/02-spec-engine-otimista.md`

Prompt:

```text
Implemente docs/specs/etapa-1/02-spec-engine-otimista.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-1/02-spec-engine-otimista.md  (a spec — a autoridade)
  docs/decisoes/etapa-1.md                       (o porque; decisoes 1, 2, 3,
                                                  9, 10, 11, 12, 16, 17 e 19..24)
  docs/projeto/estrategias.md                    (o mecanismo)
  docs/projeto/api.md                            (o contrato publicado)

ATENCAO: a spec emenda estrategias.md e api.md em quatro pontos, todos
registrados nas decisoes 19 a 24. Onde os dois discordarem, a spec vence:
  - caminho feliz e CTE de um statement, nao Begin/Commit    (decisao 19)
  - classify e NotFound -> Closed -> Conflict -> TooLow       (decisao 20)
  - Current NAO e preenchido em NotFound nem em Invalid      (decisao 21)
  - o relogio de ends_at e o now() do Postgres               (decisao 22)

Escopo: apenas RF01..RF08. NAO implemente engine pessimista, engine shard,
idempotencia, Redis, cmd/checker nem k6 — sao das specs e etapas seguintes.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- O schema em docs/projeto/schema.md e literal: nao "melhore" colunas,
  restricoes ou nomes. Se algo parecer errado, pare e pergunte.
- Nenhum switch por nome de estrategia fora de internal/app.
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Nao declare
  aceite sem a saida do comando.
- Se estourar o budget de 25 arquivos / ~800 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
internal/bid/engine.go
internal/bid/outcome.go
internal/bid/optimistic/optimistic.go
internal/bid/optimistic/sql.go
internal/bid/enginetest/conformance.go
internal/app/engine.go
internal/httpapi/bids.go
internal/httpapi/auctions.go
internal/httpapi/render.go
internal/httpapi/identity.go
internal/metrics/bid.go
internal/store/auctions.go
```

Editar:

```text
internal/httpapi/router.go   (registrar as tres rotas e o middleware)
cmd/auctiond/main.go         (app.NewEngine, abortar se falhar)
```

`internal/store/auctions.go` existe porque `POST /auctions` e `GET /auctions/:id` precisam falar com o banco sem passar pela engine — são leitura e criação, não lance. Pôr esse SQL dentro de `internal/bid` faria o contrato das três engines carregar duas rotas que nenhuma delas serve.

## Testes

Adicionar:

```text
internal/bid/enginetest/conformance.go        a suite (nao _test.go: e importada)
internal/bid/optimistic/optimistic_test.go    RunConformance + Conflict + Invalid
internal/bid/engine_test.go                   MinNextBid, IsClosed, Outcome.String
internal/httpapi/bids_test.go                 os 7 desfechos com engine falsa
internal/httpapi/auctions_test.go             POST validacoes, GET status derivado
internal/metrics/bid_test.go                  decorator observa duracao e outcome
internal/app/engine_test.go                   optimistic ok, shard e desconhecido erram
```

`bids_test.go` usa uma `BidEngine` falsa em memória, não Postgres: o que se testa ali é a tabela de mapeamento entre `Outcome` e resposta HTTP, e amarrá-la a um contêiner tornaria lento um teste que é puro `switch`.

## Checkpoints Mensuraveis

### C1 - Um lance aceito, ponta a ponta

```bash
make up && sleep 15
make seed AUCTIONS=1 TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)

curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H 'Content-Type: application/json' \
  -d '{"amountCents":500,"expectedVersion":0}'

docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT seq, amount_cents FROM bids WHERE auction_id = '$AID';"
docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT version, highest_bid_cents FROM auctions WHERE id = '$AID';"
```

Aceite:

- `201`, com `seq: 1`, `currentVersion: 1`, `currentHighestBid: 500`, `minNextBid: 600` e um `bidId` UUID
- `bids` tem exatamente uma linha, `1|500`
- `auctions` reporta `1|500`
- A linha existe **antes** de o `curl` retornar: o `201` significa durável (decisão 8)

### C2 - Os sete desfechos, cada um com seu corpo

```bash
# 409 — versao desatualizada, valor suficiente
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900,"expectedVersion":0}'
# 422 — versao correta, valor abaixo do minimo
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":550,"expectedVersion":1}'
# 400 — sem expectedVersion, rodando otimista
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900}'
# 400 — sem identidade
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -d '{"amountCents":900,"expectedVersion":1}'
# 404 — leilao inexistente
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$(uuidgen)/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900,"expectedVersion":0}'
# 410 — leilao vencido
make seed AUCTIONS=1 ENDS_IN=-1m OUT=bench/expired.json
export EID=$(jq -r '.[0].id' bench/expired.json)
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$EID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900,"expectedVersion":0}'
```

Aceite:

- Os códigos saem na ordem `409`, `422`, `400`, `400`, `404`, `410`
- `409` traz `version_conflict` e `retryable: true`; `422` traz `too_low` e `retryable: true`; `410` traz `auction_closed` e `retryable: false`
- Os três carregam `currentVersion`, `currentHighestBid` e `minNextBid`
- `404`, e os dois `400`, **não** carregam nenhum desses três campos
- O `409` prova a precedência da decisão 20: valor suficiente, versão velha, e o desfecho é conflito

### C3 - Um statement no caminho feliz, dois no de falha

```bash
docker compose exec -T postgres psql -U auction -d auction \
  -c "ALTER SYSTEM SET log_statement = 'all';" -c "SELECT pg_reload_conf();"
sleep 2

curl -s -o /dev/null -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":2000,"expectedVersion":1}'
docker compose logs postgres --since 5s | grep -c 'statement:'

curl -s -o /dev/null -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":2000,"expectedVersion":1}'
docker compose logs postgres --since 5s | grep -c 'statement:'

docker compose exec -T postgres psql -U auction -d auction \
  -c "ALTER SYSTEM RESET log_statement;" -c "SELECT pg_reload_conf();"
```

Aceite:

- O lance aceito produz **1** statement, e nenhum `BEGIN` ou `COMMIT` aparece no log
- O lance rejeitado produz **2**: a CTE e o `SELECT` do `classify`
- Um `BEGIN` no log reprova o checkpoint: o caminho feliz voltou a ser transação (decisão 19)

### C4 - Concorrencia real, historia reexecutavel

```bash
go test ./internal/bid/... -run Conformance -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
```

Aceite:

- O caso de concorrência da suíte passa com `-race`
- O replay confirma `seq` formando `1..A` sem buraco, cada `amount_cents` respeitando o incremento contra o anterior, `A` igual aos `Accepted` contados, e `auctions.version = A`
- Toda a suíte passa; `gofmt -l .` vazio e `go vet` sem saída

### C5 - Metricas de lance expostas e raspadas

```bash
curl -s localhost:8080/metrics | grep -E '^bid_'
curl -s 'localhost:9090/api/v1/query?query=sum(bid_outcomes_total)%20by%20(outcome)' \
  | jq -r '.data.result[] | "\(.metric.outcome) \(.value[1])"'
curl -s localhost:8080/metrics | grep -c 'bid_confirm_duration_seconds_bucket'
```

Aceite:

- `bid_outcomes_total` aparece com o label `strategy="optimistic"` e um label `outcome` por desfecho já produzido
- `bid_confirm_duration_seconds` expõe `_bucket`, `_sum` e `_count`, com bucket máximo cobrindo ao menos 8 segundos
- O Prometheus devolve as séries por `outcome`, com as contagens batendo com os desfechos provocados em C1 e C2
- Nenhuma métrica de idempotência ou de lote aparece: elas chegam nas etapas 2 e 3

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2 instalados
Portas livres: 5432, 6379, 8080, 9090, 3000
Repositorio limpo, .env criado a partir de .env.example
jq e uuidgen disponiveis
```

Passos:

```bash
make up && sleep 15

# criar pela API, nao pelo seed
curl -s -X POST localhost:8080/auctions -H 'Content-Type: application/json' \
  -d '{"title":"Smoke","startingBidCents":0,"minIncrementCents":100,
       "endsAt":"'$(date -u -d '+5 min' +%Y-%m-%dT%H:%M:%SZ)'"}' | jq

export AID=<id devolvido>
export UID=$(uuidgen)

# subir o preco tres vezes, sempre re-mirando pelo minNextBid da resposta
curl -s -X POST localhost:8080/auctions/$AID/bids -H "X-User-Id: $UID" \
  -d '{"amountCents":100,"expectedVersion":0}' | jq
curl -s -X POST localhost:8080/auctions/$AID/bids -H "X-User-Id: $UID" \
  -d '{"amountCents":200,"expectedVersion":1}' | jq
curl -s -X POST localhost:8080/auctions/$AID/bids -H "X-User-Id: $UID" \
  -d '{"amountCents":300,"expectedVersion":2}' | jq

curl -s localhost:8080/auctions/$AID | jq
curl -s localhost:8080/readyz | jq

make run STRATEGY=shard && sleep 5 && docker compose logs auctiond | tail -3
make run STRATEGY=optimistic && sleep 10
make down
```

Aceite manual:

- `POST /auctions` devolve `version: 0` e `minNextBid: 100`
- Os três lances devolvem `201` com `seq` 1, 2 e 3, e o `minNextBid` de cada resposta é exatamente o que a próxima usa
- `GET /auctions/:id` reporta `version: 3`, `currentHighestBid: 300`, `minNextBid: 400` e `status: "open"`
- `STRATEGY=shard` **não** sobe: o `auctiond` sai com erro nomeando a etapa 3, em vez de subir e falhar no primeiro lance
- Voltando para `optimistic`, `/readyz` responde `200` de novo
- `make down` derruba tudo sem contêiner órfão

## Definicao De Pronto

- RF01 a RF08 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- Todos os testes de `## Testes` passando com `-race`; `go vet ./...` limpo e `gofmt -l .` vazio
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado
- Nenhum `switch` por nome de estratégia fora de `internal/app`
- Nada de idempotência, Redis, engine pessimista ou shard no diff
