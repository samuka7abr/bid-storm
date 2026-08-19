# Etapa 2 — Spec 01: Engine pessimista

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [etapa 1](../etapa-1/02-spec-engine-otimista.md)

## Contexto

A etapa 1 fechou com uma engine medida: contrato publicado, envelope uniforme, suíte de conformidade contra a interface, `cmd/checker` barrando a matriz e uma célula reprodutível ponta a ponta. O que existe hoje é **uma curva**, e uma curva sozinha não cruza com nada.

Esta spec entrega a segunda. O código já reserva o lugar dela: `internal/app/engine.go` responde a `BID_STRATEGY=pessimistic` com *"the pessimistic engine arrives in etapa 2"*, e falha no boot em vez de subir sem engine.

A aposta feita na decisão 11 é cobrada aqui. A suíte de conformidade foi escrita contra `BidEngine` e nunca contra SQL justamente para que a segunda engine custasse **duas linhas de teste** — ela passa no contrato que já existe, ou está errada. Se essa promessa não se cumprir nesta spec, ela não vai se cumprir na etapa 3, e é melhor descobrir agora, com uma engine para reescrever em vez de duas.

Sustentam as decisões abaixo a **7** (pool idêntico nas três, ambiente fixado), a **9** (`409` e `422` são o mesmo evento, e o cliente trata os dois igual), a **11** (conformidade contra a interface), a **12** (`ends_at` é a guarda), a **16** (métrica entra junto da engine que a alimenta), a **19** (transação é custo do pessimismo, e o otimista não ganha uma de graça), a **22** (o relógio do Postgres é a autoridade) e a **23** (as três são medidas de uma fronteira só), mais as decisões **25** a **30**, tomadas nesta spec.

## Objetivo

Pôr a segunda estratégia sob a mesma régua da primeira, sem que uma linha do contrato HTTP mude para acomodá-la.

O sistema deve:

- Implementar `BidEngine` com `SELECT ... FOR UPDATE` dentro de transação, ignorando `ExpectedVersion` e sem nunca produzir `Conflict`
- Passar na suíte de conformidade que já existe, sem alteração na suíte
- Expor a espera em lock como série própria, medida de dentro, e legível **contra** `bid_confirm_duration_seconds`
- Subir com `BID_STRATEGY=pessimistic` e rodar uma célula de benchmark completa com o `cmd/checker` verde
- Fazer isso **sem tocar em `internal/httpapi`, em `bench/`, em `migrations/` nem em `cmd/`**: se a segunda engine exigir mudança em qualquer um deles, o contrato da etapa 1 está errado e é isso que precisa ser reportado

## Fora de Escopo

- Idempotência, `X-Idempotency-Key`, Redis e `idempotency_hits_total` — spec 02 desta etapa. A coluna `idempotency_key` continua `NULL` em toda linha escrita aqui
- `bid_attempts_per_accept` no servidor — spec 02, quando a chave de idempotência der o fio que correlaciona três requisições como tentativas do mesmo lance (decisão 16)
- Duplicatas injetadas no k6 e o invariante de chave repetida no checker — spec 03 desta etapa
- Engine single-writer — etapa 3. `BID_STRATEGY=shard` continua falhando no boot nomeando a etapa
- Sweep de `DB_POOL_SIZE` — etapa 5. Esta spec roda no pool único da decisão 7, e é o resultado dela que torna o sweep interessante
- Qualquer alteração em `internal/httpapi`, `internal/store`, `bench/`, `migrations/`, `cmd/auctiond`, `cmd/seed` ou `cmd/checker`
- Dashboards do Grafana — etapa 5

## Fluxo

```text
POST /auctions/:id/bids            ← nenhum handler muda: nenhum sabe qual engine responde
  └── app.Engine.PlaceBid
        └── instrumented (decorator)          ← mesma fronteira das três (decisão 23)
              ├── t := now
              ├── pessimistic.PlaceBid
              │     ├── pool.Begin                                    (1 rt · conexão presa até o fim)
              │     ├── SELECT ..., clock_timestamp() FOR UPDATE      (1 rt · observado)
              │     │     └── lock_wait_duration_seconds.Observe
              │     │     ├── ErrNoRows            → NotFound         rollback
              │     │     ├── IsClosed(dbNow)      → Closed           rollback
              │     │     └── Amount < MinNextBid  → TooLow           rollback
              │     ├── CTE: UPDATE ... RETURNING + INSERT bids       (1 rt)
              │     └── Commit                                        (1 rt) → Accepted
              ├── bid_confirm_duration_seconds{strategy="pessimistic"}.Observe
              └── bid_outcomes_total{strategy="pessimistic",outcome}.Inc
        │
        └── handler: a mesma tabela de mapeamento da etapa 1
              Accepted → 201 · TooLow → 422 · Closed → 410 · NotFound → 404 · error → 503
              Conflict e Invalid nunca chegam: a engine não os produz
```

`ExpectedVersion` não aparece no fluxo porque a engine **nunca lê o campo**. Não é que ela leia e perdoe uma versão velha: o lock já garante que o estado lido é o vigente, e a versão do cliente não tem função alguma na decisão.

## Decisoes Tecnicas

### A transação é paga por inteiro, e nada além dela

Quatro round-trips no aceite, três na rejeição, conexão presa do `Begin` ao `Commit`.

Existe uma forma de escrever isto em um statement só, com `FOR UPDATE` dentro de uma CTE, e ela funciona. Mas o que ela implementa não é pessimismo: o lock nasce e morre dentro do statement, a decisão volta para dentro do SQL, e o que sobra é a engine otimista com outro mecanismo de serialização. O projeto não pergunta *qual SQL é mais rápido*; pergunta o que acontece quando **um cliente segura um lock enquanto decide**, e é essa forma que a etapa 5 vai medir (decisão 25).

O que não é do mecanismo, porém, sai: `UPDATE` e `INSERT` vão numa CTE só, do mesmo formato que o otimista já usa, em vez de dois round-trips.

```sql
-- lê e tranca. clock_timestamp(), não now(): ver abaixo.
SELECT version, highest_bid_cents, min_increment_cents, status, ends_at, clock_timestamp()
  FROM auctions
 WHERE id = $1
   FOR UPDATE

-- escreve. Sem guarda de version, de status ou de ends_at: o lock é a guarda.
WITH upd AS (
    UPDATE auctions
       SET highest_bid_cents = $1,
           highest_bidder    = $2,
           version           = version + 1
     WHERE id = $3
    RETURNING version
),
ins AS (
    INSERT INTO bids (id, auction_id, user_id, amount_cents, seq)
    SELECT $4, $3, $2, $1, upd.version FROM upd
)
SELECT version FROM upd
```

Repetir as guardas no `UPDATE` seria cinto e suspensório com um custo escondido: criaria um ramo de zero linhas que ninguém sabe classificar, porque sob lock ele é inalcançável. Ramo inalcançável não é segurança, é código que nunca foi testado esperando a primeira ocasião.

O `min_increment_cents` não volta na escrita: ele já veio na leitura trancada, e a linha não pode ter mudado no meio.

`defer tx.Rollback(ctx)` depois de um `Commit` bem-sucedido não custa round-trip — o pgx marca a transação como encerrada e devolve `ErrTxClosed` sem tocar na rede. Quem for contar statements no log do Postgres vai achar quatro, não cinco.

### O relógio é o do Postgres, mas a função muda

A decisão 22 fixou que `ends_at` é decidido pelo relógio do banco. Dentro de transação, `now()` é `transaction_timestamp()` — o instante do `BEGIN`, que acontece **antes** da espera em lock.

Sob mil VUs, a transação pode passar centenas de milissegundos na fila e ainda ler um `now()` anterior à espera: a guarda de fechamento da pessimista ficaria mais frouxa que a do otimista, e a diferença apareceria na borda do `ends_at`, que é o cenário inteiro do projeto. Pareceria diferença entre estratégias e seria diferença entre relógios.

Por isso a leitura trancada devolve `clock_timestamp()`, avaliado durante a execução do statement, depois da espera. `IsClosed` não muda — muda qual instante o alimenta (decisão 27).

Isso deixa `bids.created_at`, que continua no `DEFAULT now()`, **anterior** ao instante que autorizou o aceite. Logo `created_at < ends_at` vale por construção sempre que o lance passa, e o invariante I4 do checker não pode acusar falso positivo por causa do lock.

### `ExpectedVersion` não é ignorado com gentileza: não é lido

A pessimista nunca produz `Conflict` nem `Invalid`. O perdedor recebe `TooLow`, que é o mesmo evento visto por outro mecanismo (decisão 9), e o apostador do k6 trata `422` exatamente como trata `409` — é o que mantém a carga equivalente entre as engines.

Duas consequências viram asserção verificável no fim de cada célula:

```
bid_outcomes_total{strategy="pessimistic",outcome="conflict"} == 0
bid_outcomes_total{strategy="pessimistic",outcome="invalid"}  == 0
```

A segunda tem um efeito colateral útil: o invariante I6 do `cmd/checker` já falha quando `invalid > 0`, tratando isso como k6 mal configurado. Sob a pessimista essa mesma checagem passa a ser, de graça, um detector de engine que voltou a olhar para `ExpectedVersion`.

### A classificação tem três desfechos, e a ordem não é escolha

`NotFound` → `Closed` → `TooLow`, decididos em Go sobre a linha trancada.

A precedência da decisão 20 não se coloca aqui: sem `Conflict`, não há dois desfechos disputando a mesma rejeição. E a classificação **não custa round-trip extra** — o estado já veio na leitura que tomou o lock. O otimista paga um `SELECT` a mais para classificar porque descobre a falha depois de tentar; a pessimista olha antes de tentar, e essa é a assimetria real entre os dois mecanismos.

### `READ COMMITTED`, e a alternativa que ficou registrada como não medida

`SERIALIZABLE` transformaria toda disputa em `40001` mais retentativa, ou seja, seria a estratégia otimista com a retentativa movida para dentro do banco — amplificação de retry, colapso sob contenção e tudo o mais que a hipótese atribui ao otimista. É uma quarta célula no eixo de estratégias, respondendo a uma pergunta que este projeto não fez (decisão 30).

O projeto não afirma nada sobre `SERIALIZABLE`. Afirmar sem medir é o que os quatro invariantes de método existem para impedir.

### Sem laço de deadlock, porque deadlock não pode acontecer

Cada transação tranca **uma** linha de `auctions` e nunca uma segunda. O `INSERT` em `bids` toma `FOR KEY SHARE` sobre a mesma linha que a transação já detém em modo mais forte, e na mesma transação. Sem duas linhas não há ordem de aquisição para inverter, e sem inversão não há ciclo (decisão 29).

Não há `lock_timeout` nem timeout de servidor, pelo mesmo motivo da etapa 1: o handler passa `c.Request.Context()` inteiro, o `net/http` cancela quando o cliente desiste por `BID_DEADLINE`, e `db_pool_canceled_acquire_total` conta. A fila é medida, não cortada — e na pessimista ela **é** o fenômeno.

### Observabilidade

Entra uma série, a que a decisão 16 aloca à etapa 2 junto desta engine:

| Série | Tipo | Labels | Buckets |
| --- | --- | --- | --- |
| `lock_wait_duration_seconds` | Histogram | nenhum | os mesmos de `bid_confirm_duration_seconds` |

**Buckets iguais, de propósito.** As duas séries só respondem à pergunta que importa se puderem ser lidas uma contra a outra, bucket a bucket: se a espera em lock domina a confirmação, os quatro round-trips da decisão 25 são ruído dentro do número, e a crítica *"você aleijou a pessimista com round-trips"* morre no gráfico em vez de no texto. Buckets diferentes empurrariam a comparação para interpolação de quantis, que é onde uma diferença de dez pontos percentuais se esconde (decisão 26).

**Sem label `strategy`.** Só uma engine produz a série; um label de valor único sugeriria que as outras duas reportam zero, quando elas não reportam nada — e zero é uma afirmação diferente de silêncio.

**O que o histograma mede, e o que não mede.** Ele cronometra o `SELECT ... FOR UPDATE` inteiro: round-trip, planejamento e espera. Não é a espera pura, e extraí-la exigiria amostrar `pg_locks` — custo no caminho quente para medir o caminho quente. Não precisa: na célula de 1000 leilões, onde ninguém disputa a mesma linha, o piso do histograma **é** o custo do round-trip. A calibração vem da própria matriz.

**A leitura que não pode ser feita com uma série só.** Com `DB_POOL_SIZE=25` e 1000 VUs, no máximo 25 transações esperam pelo lock ao mesmo tempo; as outras 975 esperam pelo **pool**. A espera não some, ela migra — de `lock_wait_duration_seconds` para `db_pool_acquire_duration_seconds_total` e `db_pool_empty_acquire_total`, que a etapa 1 já expõe. O custo de um lance na pessimista é a soma dos dois, e olhar só para um conta metade da história. É também o que torna o sweep de pool da etapa 5 uma pergunta de verdade: mover o teto move a espera entre as duas séries, e não é óbvio que mova o throughput.

`bid_confirm_duration_seconds` e `bid_outcomes_total` continuam vindo do decorator, sem alteração. `lock_wait_duration_seconds` é a segunda exceção registrada da decisão 23, e o limite dela é o mesmo da primeira: o que compara as três estratégias é medido num lugar só; o que descreve o mecanismo de uma delas é medido dentro dela (decisão 28).

## Requisitos Funcionais

### RF01 - Engine pessimista

`internal/bid/pessimistic` implementa `BidEngine` com transação explícita no isolamento padrão:

1. `pool.Begin(ctx)`, com `defer tx.Rollback(ctx)`
2. `SELECT version, highest_bid_cents, min_increment_cents, status, ends_at, clock_timestamp() FROM auctions WHERE id = $1 FOR UPDATE`
3. Classificação em Go, na ordem `NotFound` → `Closed` → `TooLow`, usando `AuctionState.IsClosed(dbNow)` e `AuctionState.MinNextBid()` — os mesmos métodos do contrato, sem reimplementar a regra
4. Escrita numa CTE só, `UPDATE ... RETURNING version` mais `INSERT INTO bids`, sem guarda redundante
5. `tx.Commit(ctx)`, e só então `Accepted`

`req.ExpectedVersion` não é lido em nenhum ponto. `req.IdempotencyKey` também não: a coluna continua `NULL` até a spec 02.

Toda rejeição devolve `error` nil. Falha de infraestrutura devolve `BidResult{}` e erro embrulhado, como no otimista.

### RF02 - Envelope montado do que foi lido

`BidResult.Current` no aceite é montado da linha trancada mais o retorno da escrita: `Version` e `Seq` são a `version` devolvida pelo `UPDATE`, `HighestBidCents` é o valor recém-aceito, `MinIncrementCents` vem da leitura trancada.

Em `Closed` e `TooLow`, `Current` é a linha trancada como lida. Em `NotFound`, fica zerado (decisão 21).

Nenhum campo do envelope vem de uma segunda consulta, e nenhum vem de um valor que a engine não leu.

### RF03 - `lock_wait_duration_seconds`

`internal/metrics` ganha `NewLockWait(reg prometheus.Registerer) prometheus.Observer`, que registra o histograma com os buckets já usados por `bid_confirm_duration_seconds`, sem labels.

`pessimistic.New(pool *pgxpool.Pool, lockWait prometheus.Observer) *Engine` recebe o observador pronto — a engine depende da interface de um método, não de `internal/metrics`, e todo nome de série continua morando em `internal/metrics`.

A observação cobre exatamente o `SELECT ... FOR UPDATE`: começa antes de enviar, termina depois do `Scan`. `Begin` e `Commit` ficam de fora, porque não é neles que se espera por lock.

Chamada que falhar com erro de infraestrutura não observa: a duração de um banco caído não é espera em lock.

### RF04 - `BID_STRATEGY=pessimistic` sobe

`internal/app.NewEngine` passa a construir a engine pessimista, embrulhada no mesmo decorator de métricas das demais, e a criar o observador de lock a partir do mesmo `prometheus.Registerer`.

`shard` continua devolvendo erro nomeando a etapa 3. Valor desconhecido continua falhando no boot. Nenhum `switch` por nome de estratégia nasce fora de `internal/app`.

`cmd/auctiond` não muda.

### RF05 - Conformidade, e o que só a pessimista prova

`internal/bid/pessimistic/pessimistic_test.go` chama `enginetest.RunConformance` sem alterar uma linha da suíte. Mais dois testes que são específicos desta engine, espelhando os que o otimista tem:

- **Versão velha com valor suficiente é aceita.** O mesmo cenário que no otimista produz `Conflict` aqui produz `Accepted`: é a prova de que `ExpectedVersion` não participa da decisão
- **`ExpectedVersion` nulo é aceito.** No otimista isso é `Invalid`; aqui o campo não existe para a engine
- **Sob concorrência, nenhum resultado é `Conflict` nem `Invalid`.** N goroutines num leilão só, e a asserção é sobre a distribuição de desfechos, não sobre o banco

Se a suíte precisar de qualquer alteração para acomodar a segunda engine, **pare e reporte**: isso significa que ela estava escrita contra o otimista disfarçada de contrato, e é um achado mais importante que esta spec.

### RF06 - O resto do sistema não muda

O diff não pode conter alteração em `internal/httpapi`, `internal/store`, `internal/bid/engine.go`, `internal/bid/outcome.go`, `internal/bid/enginetest/`, `internal/bid/optimistic/`, `cmd/`, `bench/`, `migrations/`, `docker-compose.yaml` nem `Makefile`.

Esse requisito é o teste real da etapa 1. A segunda engine entra por um ponto de extensão que já existia, ou o contrato publicado estava errado.

## Requisitos Nao Funcionais

- Nenhuma dependência de runtime nova: `pgx/v5`, `gin`, `prometheus/client_golang` e `google/uuid` continuam sendo tudo. Redis entra na spec 02
- `internal/bid/pessimistic` importa `internal/bid`, `pgx`, `pgxpool`, `uuid` e `prometheus` — e nada mais. Não importa `internal/metrics`, `internal/app` nem Gin
- `go vet ./...` limpo; `gofmt -l .` vazio
- A suíte de conformidade da pessimista roda em menos de 30 segundos, contêiner incluído
- Toda rejeição é `nil` no `error` da engine, sem exceção
- Nenhum log por requisição: continua valendo o de `503` e o de boot

## Budget do PR

Até 8 arquivos e aproximadamente 350 linhas de código próprio.

É um terço do budget da spec 02 da etapa 1, e essa é a medida do que a decisão 11 comprou: o contrato, o envelope, o decorator, a suíte e o harness já existem, então a segunda engine é engine e mais nada. Se o PR passar de 8 arquivos ou 350 linhas, alguma coisa está sendo reescrita que não devia — **pare e reporte** em vez de continuar.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: medio
- Referencia permitida: `docs/projeto/estrategias.md`, `docs/projeto/api.md`, `docs/projeto/schema.md`, `docs/projeto/observabilidade.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/specs/etapa-1/02-spec-engine-otimista.md`, `docs/specs/etapa-2/01-spec-engine-pessimista.md`

Prompt:

```text
Implemente docs/specs/etapa-2/01-spec-engine-pessimista.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-2/01-spec-engine-pessimista.md  (a spec — a autoridade)
  docs/decisoes/etapa-2.md                         (o porque; decisoes 25 a 30)
  docs/decisoes/etapa-1.md                         (decisoes 7, 9, 11, 12, 16,
                                                    19, 21, 22 e 23)
  internal/bid/optimistic/                         (a engine irma, ja pronta)
  internal/bid/enginetest/conformance.go           (o contrato executavel)

ATENCAO: a spec emenda estrategias.md e a decisao 22 em dois pontos:
  - a leitura trancada devolve clock_timestamp(), NAO now(): dentro de
    transacao now() e o instante do BEGIN, anterior a espera em lock
    (decisao 27)
  - UPDATE e INSERT vao numa CTE so, e o UPDATE nao repete guarda de
    version, status ou ends_at: o lock e a guarda (decisao 25)

Escopo: apenas RF01..RF06. NAO implemente idempotencia, Redis,
X-Idempotency-Key, bid_attempts_per_accept no servidor, duplicatas no k6 nem
engine shard — sao das specs e etapas seguintes.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere internal/httpapi, internal/store, internal/bid/engine.go,
  internal/bid/outcome.go, internal/bid/enginetest/, internal/bid/optimistic/,
  cmd/, bench/, migrations/, docker-compose.yaml nem Makefile. Se algum deles
  parecer precisar de mudanca, pare e reporte: e um achado sobre o contrato da
  etapa 1, nao uma tarefa desta spec.
- A suite de conformidade NAO pode ser alterada. A engine passa nela como ela
  esta, ou esta errada.
- Nenhum switch por nome de estrategia fora de internal/app.
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Nao declare
  aceite sem a saida do comando.
- Se estourar o budget de 8 arquivos / ~350 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
internal/bid/pessimistic/pessimistic.go
internal/bid/pessimistic/sql.go
internal/metrics/lock.go
```

Editar:

```text
internal/app/engine.go   (construir a engine; shard continua nomeando a etapa 3)
```

`internal/metrics/lock.go` existe em vez de a engine declarar o próprio histograma para que todo nome de série continue morando num pacote só — é lá que um revisor procura quando quer saber o que este processo publica. A engine recebe um `prometheus.Observer` e não sabe o nome da série que alimenta.

## Testes

Adicionar:

```text
internal/bid/pessimistic/pessimistic_test.go   RunConformance + os tres casos de RF05
internal/metrics/lock_test.go                  a serie registra, observa e tem os buckets de confirm
```

Editar:

```text
internal/app/engine_test.go   pessimistic agora constroi; shard e desconhecido continuam errando
```

## Checkpoints Mensuraveis

### C1 - Um lance aceito sem `expectedVersion`

```bash
make up && sleep 15
make run STRATEGY=pessimistic && sleep 10
curl -s localhost:8080/metrics | grep -c 'strategy="pessimistic"'

make seed AUCTIONS=1 TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)

# sem expectedVersion no corpo: no otimista isto e 400 invalid
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H 'Content-Type: application/json' \
  -d '{"amountCents":500}'

docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT seq, amount_cents, idempotency_key IS NULL FROM bids WHERE auction_id = '$AID';"
```

Aceite:

- `/metrics` já traz séries com `strategy="pessimistic"` antes do primeiro lance: o decorator liga o label no boot
- `201`, com `seq: 1`, `currentVersion: 1`, `currentHighestBid: 500` e `minNextBid: 600`
- `bids` tem exatamente uma linha, `1|500|t` — e a chave de idempotência continua `NULL`
- A linha existe **antes** de o `curl` retornar (decisão 8)

### C2 - Os desfechos que esta engine produz, e o que ela nunca produz

```bash
# versao velha e valor suficiente: no otimista isto e 409
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900,"expectedVersion":0}'
# valor abaixo do minimo
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":100,"expectedVersion":2}'
# sem identidade — o middleware nao muda
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -d '{"amountCents":9000}'
# leilao inexistente
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$(uuidgen)/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900}'
# leilao vencido
make seed AUCTIONS=1 ENDS_IN=-1m OUT=bench/expired.json
export EID=$(jq -r '.[0].id' bench/expired.json)
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$EID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":900}'

curl -s localhost:8080/metrics | grep 'bid_outcomes_total'
```

Aceite:

- Os códigos saem na ordem `201`, `422`, `400`, `404`, `410`
- O primeiro é `201` com versão velha e valor suficiente — a prova de RF01: `ExpectedVersion` não participa da decisão
- `422` traz `too_low`, `retryable: true`, `currentVersion`, `currentHighestBid` e `minNextBid`; `410` traz `auction_closed` e `retryable: false`; `404` não traz nenhum dos três campos de estado
- `bid_outcomes_total` **não** tem série com `outcome="conflict"` nem com `outcome="invalid"`, em nenhum valor

### C3 - A espera em lock existe, e sai na métrica

```bash
curl -s localhost:8080/metrics | grep 'lock_wait_duration_seconds_sum'

# uma sessao externa segura a linha por 3s
docker compose exec -T postgres psql -U auction -d auction -c \
  "BEGIN; SELECT id FROM auctions WHERE id='$AID' FOR UPDATE; SELECT pg_sleep(3); COMMIT;" &
sleep 1

time curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":9000}'
wait

curl -s localhost:8080/metrics | grep -E 'lock_wait_duration_seconds_(sum|count)'
curl -s localhost:8080/metrics | grep 'bid_confirm_duration_seconds_sum{strategy="pessimistic"}'
```

Aceite:

- O `curl` bloqueia por aproximadamente 2 segundos e devolve `201`: a engine esperou o lock em vez de falhar
- `lock_wait_duration_seconds_sum` cresce em ~2 segundos com esse único lance
- `bid_confirm_duration_seconds_sum{strategy="pessimistic"}` cresce em quantidade parecida, e a diferença entre os dois é a parte que não é espera — é essa diferença que a decisão 26 existe para tornar legível
- Os buckets das duas séries são os mesmos: `curl -s localhost:8080/metrics | grep -c 'lock_wait_duration_seconds_bucket'` bate com a contagem de buckets de `bid_confirm_duration_seconds`

### C4 - Conformidade, sem tocar na suíte

```bash
git diff --name-only internal/bid/enginetest/ internal/bid/optimistic/ internal/httpapi/ cmd/ bench/ migrations/
go test ./internal/bid/pessimistic/... -race -count=1 -v
go test ./... -race -count=1
gofmt -l . && go vet ./...
```

Aceite:

- O `git diff --name-only` não lista **nenhum** arquivo: RF06 cumprido
- `RunConformance` passa inteira com `-race`, incluindo o replay do caso de concorrência
- Os três testes específicos de RF05 passam: versão velha aceita, `expectedVersion` nulo aceito, e nenhum `Conflict` ou `Invalid` sob concorrência
- Toda a suíte passa; `gofmt -l .` vazio e `go vet` sem saída

### C5 - Uma célula real, com o checker verde

```bash
make run STRATEGY=pessimistic && sleep 10
make bench RUN=e2c5-pess STRATEGY=pessimistic AUCTIONS=1 POLICY=immediate SCENARIO=ramp
echo "checker exit: $?"

make run STRATEGY=optimistic && sleep 10
make bench RUN=e2c5-otim STRATEGY=optimistic AUCTIONS=1 POLICY=immediate SCENARIO=ramp

jq -r '.cell.strategy' bench/results/e2c5-pess/env.json
jq -r '{accepted,conflict,outbid,invalid,exhausted}' bench/results/e2c5-pess/client.json
cat bench/results/e2c5-pess/checker.txt
```

Aceite:

- `make bench` da pessimista termina com código 0 e os seis invariantes verdes, sem alteração em `bench/` nem em `cmd/checker`
- `env.json` grava `strategy: "pessimistic"`; o `run-cell.sh` só chegou até aqui porque confirmou pelo `/metrics` que o processo roda essa engine
- `client.json` reporta `conflict: 0` e `invalid: 0`, e `outbid` maior que zero: o perdedor apareceu como `422`, não como `409`
- As duas células ficam gravadas em `bench/results/` como par comparável. **Nenhuma conclusão sobre qual venceu entra no PR**: uma célula não é a matriz, e a comparação é da etapa 5

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
make run STRATEGY=pessimistic && sleep 10

curl -s -X POST localhost:8080/auctions -H 'Content-Type: application/json' \
  -d '{"title":"Smoke pessimista","startingBidCents":0,"minIncrementCents":100,
       "endsAt":"'$(date -u -d '+5 min' +%Y-%m-%dT%H:%M:%SZ)'"}' | jq

export AID=<id devolvido>
export UID=$(uuidgen)

# tres lances sem nunca mandar expectedVersion
for cents in 100 200 300; do
  curl -s -X POST localhost:8080/auctions/$AID/bids -H "X-User-Id: $UID" \
    -d "{\"amountCents\":$cents}" | jq -c
done

curl -s localhost:8080/auctions/$AID | jq
curl -s localhost:8080/readyz | jq

make run STRATEGY=shard && sleep 5 && docker compose logs auctiond | tail -3
make run STRATEGY=optimistic && sleep 10
curl -s -X POST localhost:8080/auctions/$AID/bids -H "X-User-Id: $UID" \
  -d '{"amountCents":9000}' -w '\n%{http_code}\n'
make down
```

Aceite manual:

- Os três lances devolvem `201` com `seq` 1, 2 e 3, sem `expectedVersion` em nenhum
- `GET /auctions/:id` reporta `version: 3`, `currentHighestBid: 300`, `minNextBid: 400` e `status: "open"`
- `STRATEGY=shard` **não** sobe: o `auctiond` sai com erro nomeando a etapa 3
- De volta em `optimistic`, o mesmo corpo sem `expectedVersion` volta a ser `400`: a diferença entre as duas engines aparece no contrato, e o handler nunca soube de nenhuma das duas
- `make down` derruba tudo sem contêiner órfão

## Definicao De Pronto

- RF01 a RF06 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- `RunConformance` passando na pessimista **sem uma linha alterada** em `internal/bid/enginetest/`
- `git diff --name-only` vazio para `internal/httpapi`, `internal/store`, `internal/bid/enginetest`, `internal/bid/optimistic`, `cmd/`, `bench/` e `migrations/`
- Todos os testes passando com `-race`; `go vet ./...` limpo e `gofmt -l .` vazio
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado
- Nada de idempotência, Redis, `X-Idempotency-Key` ou engine shard no diff
