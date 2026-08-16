# Etapa 1 — Spec 01: Fundação

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md)

## Contexto

O repositório tem apenas documentação: nenhum `go.mod`, nenhum código, `Makefile` e `docker-compose.yaml` vazios. Esta é a primeira spec da etapa 1 e a primeira linha de código do projeto.

A etapa 1 inteira entrega schema, migrations, compose, API Gin, engine otimista, suíte de conformidade, `cmd/seed` e `cmd/checker`. Isso é grande demais para um PR. Esta spec entrega só a **fundação de dados e o esqueleto do processo**, de forma que a spec 02 precise implementar apenas `BidEngine` e o handler de lance, contra um banco que já existe e um processo que já sobe.

O que sustenta as decisões abaixo está em [decisoes/etapa-1.md](../../decisoes/etapa-1.md) — em especial as decisões **3** (`UNIQUE (auction_id, seq)`), **7** (recursos fixados e pool idêntico), **12** (`ends_at` como guarda), **13** (isolamento entre células), **15** (`idempotency_key` já na `001`) e **17** (`min_increment_cents` é do leilão).

## Objetivo

Erguer a base sobre a qual as três engines serão comparadas, com o ambiente fixado o suficiente para que os números do benchmark signifiquem alguma coisa.

O sistema deve:

- Subir Postgres, Redis, Prometheus e Grafana por `docker compose`, com limites de CPU e memória **verificáveis**, não apenas declarados
- Aplicar a migration `001` por um serviço que roda até completar antes do `auctiond` subir
- Expor um `auctiond` que conecta no pool, serve `/healthz`, `/readyz` e `/metrics`, e **recusa servir** se a versão do schema não for a esperada
- Publicar as métricas do pool `pgx`, que são a prova de que a fila de uma engine não se formou no pool
- Semear N leilões em tempo desprezível, com manifesto legível pelo k6

## Fora de Escopo

- `BidEngine`, `optimistic.go`, `POST /auctions/:id/bids` — spec 02
- `POST /auctions` e `GET /auctions/:id` — spec 02, junto com o envelope de resposta
- Middleware de idempotência e uso do Redis — etapa 2. Nesta spec o Redis apenas sobe e responde `PING`
- `cmd/checker`, suíte de conformidade de engine, k6 — specs seguintes
- Dashboards do Grafana — etapa 5. Aqui provisiona-se apenas o datasource
- `closerd`, WebSocket, painel React

## Fluxo

```text
make up
  ├── postgres    (healthcheck: pg_isready)
  ├── redis       (healthcheck: redis-cli ping)
  ├── migrate     depends_on postgres:service_healthy
  │                 └── aplica 000001_init → schema_migrations.version = 1
  ├── auctiond    depends_on migrate:service_completed_successfully
  │                 ├── conecta pgxpool (DB_POOL_SIZE)
  │                 ├── lê schema_migrations
  │                 │     ├── dirty=true            → /readyz 503, não serve tráfego
  │                 │     ├── version != 1          → /readyz 503, log com esperado vs encontrado
  │                 │     └── version == 1          → /readyz 200
  │                 └── /metrics expõe db_pool_* + runtime Go
  ├── prometheus  raspa auctiond:8080/metrics a cada 5s
  └── grafana     datasource Prometheus provisionado

make seed AUCTIONS=1000
  └── cmd/seed  ── CopyFrom ──> auctions (1000 linhas)
                └── escreve bench/auctions.json  [{id, version, minNextBid}, ...]
```

## Decisoes Tecnicas

### Migrations e fail-fast de schema

`golang-migrate` pela imagem oficial `migrate/migrate` como serviço do compose. Nenhuma dependência Go em `golang-migrate` — o `auctiond` só precisa **ler** `schema_migrations`, o que é uma query comum.

`internal/db.ExpectedSchemaVersion = 1` é constante no código. O `auctiond` compara no boot e a cada chamada de `/readyz`. Divergência não é aviso: `/readyz` devolve `503` e o processo loga a versão esperada e a encontrada.

O motivo é específico deste projeto: servir requisições contra um banco de formato diferente produziria um benchmark inexplicável, e o erro só apareceria como um número estranho na tabela de resultados três horas depois.

### Recursos fixados, e verificados

Limites de CPU e memória no compose para Postgres e `auctiond`. `DB_POOL_SIZE` é uma variável só, aplicada igualmente a qualquer estratégia (decisão 7).

Declarar `deploy.resources.limits` não é o mesmo que o limite existir: dependendo da versão do Compose, a chave pode ser silenciosamente ignorada. Por isso o aceite do checkpoint C1 é `docker inspect`, não a leitura do YAML. A comparabilidade das três estratégias depende desse limite ser real.

O `auctiond` roda **dentro** do compose, com `Dockerfile` próprio. Rodar no host durante o benchmark deixaria o processo sob teste sem limite algum.

### Semeadura

`cmd/seed` usa `pgx.CopyFrom`, não `INSERT` em laço: semear 1000 leilões precisa ser desprezível diante da célula do benchmark, senão o tempo de preparação vaza para dentro da medição.

O manifesto `bench/auctions.json` carrega `id`, `version` e `minNextBid` de cada leilão recém-criado. Com ele, o `setup()` do k6 não precisa de nenhum `GET` — o que importa porque a contenção alta usa 1 leilão mas a contenção baixa usa 1000, e mil requisições de preparação por célula seriam ruído puro.

A flag `-truncate` executa `TRUNCATE bids, auctions RESTART IDENTITY CASCADE` antes de semear, que é o primeiro passo de cada célula da matriz (decisão 13).

### Observabilidade

Só entram as métricas que têm o que medir nesta spec: as do pool e as de runtime do Go. As de lance chegam com a engine, na spec 02 (decisão 16).

`pgxpool` não expõe duração por aquisição, então o histograma citado em [observabilidade.md](../../projeto/observabilidade.md) não é implementável honestamente. O que existe em `pool.Stat()` é acumulado, e é suficiente:

| Série | Tipo | De onde |
| --- | --- | --- |
| `db_pool_conns{state="acquired\|idle\|max"}` | Gauge | `AcquiredConns()`, `IdleConns()`, `MaxConns()` |
| `db_pool_acquire_total` | Counter | `AcquireCount()` |
| `db_pool_acquire_duration_seconds_total` | Counter | `AcquireDuration()` |
| `db_pool_empty_acquire_total` | Counter | `EmptyAcquireCount()` |
| `db_pool_canceled_acquire_total` | Counter | `CanceledAcquireCount()` |

`rate(db_pool_acquire_duration_seconds_total) / rate(db_pool_acquire_total)` dá a espera média por aquisição, e `db_pool_empty_acquire_total` é o sinal direto de pool saturado — que é exatamente o que precisa ser separado do custo de lock quando a engine pessimista entrar.

Implementado como um `prometheus.Collector` que lê `pool.Stat()` no momento do scrape. Sem goroutine de coleta.

### Módulo Go

`github.com/samuka7abr/auction-system`, em minúsculas, ainda que o repositório seja `Auction-System`. O projeto é um binário, nunca uma dependência importada, e um path com maiúsculas produz escapes `!a!uction` no proxy de módulos e imports feios em todo arquivo.

## Requisitos Funcionais

### RF01 - Compose com recursos fixados

`docker-compose.yaml` na raiz define `postgres`, `redis`, `migrate`, `auctiond`, `prometheus` e `grafana`.

Postgres com `max_connections=200`, `shared_buffers=512MB`, `synchronous_commit=on`, limite de `2.0` CPUs e `2G`. `auctiond` com limite de `2.0` CPUs e `1G`. `postgres` e `redis` com healthcheck. `migrate` depende de `postgres` saudável; `auctiond` depende de `migrate` ter completado com sucesso.

Nenhuma senha literal no arquivo: tudo por variável, com `.env.example` versionado e `.env` no `.gitignore`.

### RF02 - Migration 001

`migrations/000001_init.up.sql` cria o tipo `auction_status`, as tabelas `auctions` e `bids`, e os índices, exatamente como em [projeto/schema.md](../../projeto/schema.md) — incluindo `UNIQUE (auction_id, seq)`, o índice único parcial em `idempotency_key` e todos os `CHECK`.

`migrations/000001_init.down.sql` reverte por completo, deixando o banco sem nenhum objeto criado pela `up`.

### RF03 - Pool e configuração

`internal/config` lê `DATABASE_URL`, `REDIS_URL`, `HTTP_ADDR`, `DB_POOL_SIZE` e `BID_STRATEGY` do ambiente, com defaults documentados, e falha no boot com mensagem clara se `DATABASE_URL` faltar.

`internal/db.NewPool` constrói o `pgxpool` com `MaxConns = DB_POOL_SIZE`. `BID_STRATEGY` é lida e logada nesta spec, mas ainda não seleciona nada — a chave existe para que a spec 02 só precise ligá-la.

### RF04 - Verificação de schema

`internal/db.CheckSchema(ctx, pool) error` consulta `SELECT version, dirty FROM schema_migrations` e retorna erro tipado distinguindo três casos: tabela ausente, `dirty = true`, e `version != ExpectedSchemaVersion`.

O `auctiond` chama no boot: falha loga em nível de erro com esperado e encontrado, e o processo continua vivo servindo `/healthz` mas com `/readyz` em `503` — para que a causa apareça no orquestrador em vez de virar um crashloop mudo.

### RF05 - Endpoints de saúde e métricas

`GET /healthz` — liveness, `200` sem tocar no banco.

`GET /readyz` — readiness, `200` apenas com pool respondendo a `Ping` e `CheckSchema` sem erro. Caso contrário `503` com corpo JSON identificando qual das duas condições falhou.

`GET /metrics` — handler do `promhttp` sobre um registry próprio (não o `DefaultRegisterer`), com o coletor do pool e os coletores de runtime Go registrados.

### RF06 - Semeadura

`cmd/seed` aceita `-auctions` (default 1), `-ends-in` (default `5m`), `-min-increment` (default 100), `-starting-bid` (default 0), `-truncate` (default false) e `-out` (default `bench/auctions.json`).

Insere via `pgx.CopyFrom` e escreve o manifesto JSON com `id`, `version` e `minNextBid` de cada leilão. Sai com código diferente de zero se o número de linhas inseridas não bater com `-auctions`.

### RF07 - Makefile

Alvos `up`, `down`, `migrate`, `migrate-down`, `seed`, `run`, `logs`, `test`, `fmt`, `lint`. `seed` repassa `AUCTIONS`; `run` repassa `STRATEGY`.

## Requisitos Nao Funcionais

- Go 1.23. Dependências de runtime limitadas a `pgx/v5`, `gin`, `prometheus/client_golang` e `google/uuid`
- `docker compose up` a partir do repositório limpo funciona sem passo manual além de copiar `.env.example` para `.env`
- Nenhum segredo literal em arquivo versionado
- Migration reversível: `up → down → up` deixa o schema idêntico, verificado por dump comparado
- `go vet ./...` limpo; `gofmt -l .` vazio
- Semear 1000 leilões em menos de 2 segundos em máquina de desenvolvimento
- Log estruturado (`log/slog`) em JSON, com `strategy`, `pool_size` e `schema_version` no evento de boot
- Encerramento gracioso em `SIGTERM`: para de aceitar conexões, drena o servidor HTTP e fecha o pool

## Budget do PR

Até 20 arquivos e aproximadamente 600 linhas de código próprio, sem contar `go.sum` e SQL de migration. Se passar disso, a spec foi mal cortada — pare e reporte em vez de continuar.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: medio
- Referencia permitida: `docs/projeto/schema.md`, `docs/projeto/arquitetura.md`, `docs/projeto/observabilidade.md`, `docs/decisoes/etapa-1.md`, `docs/specs/etapa-1/01-spec-fundacao.md`

Prompt:

```text
Implemente docs/specs/etapa-1/01-spec-fundacao.md no repositorio Auction-System.

Leia antes de comecar:
  docs/specs/etapa-1/01-spec-fundacao.md   (a spec — a autoridade)
  docs/projeto/schema.md                   (schema, literal)
  docs/decisoes/etapa-1.md                 (o porque; decisoes 3, 7, 12, 13, 15, 16, 17)

Escopo: apenas RF01..RF07. NAO implemente BidEngine, engine otimista,
POST /auctions, GET /auctions/:id nem POST /auctions/:id/bids — sao da spec 02.

Regras:
- Modulo: github.com/samuka7abr/auction-system
- O schema em docs/projeto/schema.md e literal: nao "melhore" colunas,
  restricoes ou nomes. Se algo parecer errado, pare e pergunte.
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Nao declare
  aceite sem a saida do comando.
- Se estourar o budget de 20 arquivos / ~600 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
go.mod
go.sum
.env.example
.gitignore
Dockerfile
migrations/000001_init.up.sql
migrations/000001_init.down.sql
cmd/auctiond/main.go
cmd/seed/main.go
internal/config/config.go
internal/db/pool.go
internal/db/schema.go
internal/httpapi/router.go
internal/httpapi/health.go
internal/metrics/registry.go
internal/metrics/pool.go
internal/testsupport/postgres.go
deploy/prometheus/prometheus.yml
deploy/grafana/provisioning/datasources/prometheus.yml
```

Editar:

```text
Makefile              (vazio hoje)
docker-compose.yaml   (vazio hoje)
```

`docker-compose.yaml` fica na raiz, onde já existe. [projeto/arquitetura.md](../../projeto/arquitetura.md) descreve a árvore com `deploy/docker-compose.yml`; a raiz prevalece, e o documento foi ajustado.

## Testes

Adicionar:

```text
internal/db/migrate_test.go       up → down → up, schema final identico (dump comparado)
internal/db/schema_test.go        CheckSchema: tabela ausente, dirty, versao errada, ok
internal/httpapi/health_test.go   /healthz 200 sem banco; /readyz 503 sem banco
internal/metrics/pool_test.go     coletor expoe as 5 series com pool real
cmd/seed/seed_test.go             1000 leiloes: contagem, ids unicos, manifesto valido
```

`internal/testsupport/postgres.go` sobe um Postgres por `testcontainers-go` e aplica as migrations, reutilizando um contêiner por pacote. É o mesmo helper que a suíte de conformidade da spec 03 vai usar — por isso nasce aqui, e não embutido num teste.

## Checkpoints Mensuraveis

### C1 - Compose sobe e os limites sao reais

```bash
cp .env.example .env
make up
sleep 15
docker compose ps
docker inspect auction-system-postgres-1 \
  --format '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}'
docker inspect auction-system-auctiond-1 \
  --format '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}'
```

Aceite:

- `docker compose ps` mostra `postgres` e `redis` como `healthy`, `migrate` como `exited (0)`, `auctiond`, `prometheus` e `grafana` como `running`
- `postgres` reporta `2000000000 2147483648`
- `auctiond` reporta `2000000000 1073741824`
- Um `0` em qualquer um dos dois reprova o checkpoint: o limite foi declarado mas não aplicado

### C2 - Migration aplica, reverte e reaplica sem drift

```bash
make migrate
docker compose exec -T postgres pg_dump -s -U auction auction > /tmp/schema-a.sql
make migrate-down
docker compose exec -T postgres psql -U auction -d auction -c '\dt'
make migrate
docker compose exec -T postgres pg_dump -s -U auction auction > /tmp/schema-b.sql
diff /tmp/schema-a.sql /tmp/schema-b.sql && echo "SEM DRIFT"
```

Aceite:

- Após `migrate-down`, `\dt` não lista `auctions` nem `bids`
- `diff` sai vazio e imprime `SEM DRIFT`
- `SELECT version, dirty FROM schema_migrations` devolve `1, false`

### C3 - Schema divergente impede servir trafego

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/readyz   # espera 200
docker compose exec -T postgres psql -U auction -d auction \
  -c 'UPDATE schema_migrations SET version = 99;'
curl -s -w '\n%{http_code}\n' localhost:8080/readyz              # espera 503
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/healthz  # espera 200
docker compose logs auctiond | tail -5
docker compose exec -T postgres psql -U auction -d auction \
  -c 'UPDATE schema_migrations SET version = 1;'
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/readyz   # volta a 200
```

Aceite:

- `/readyz` alterna `200 → 503 → 200` conforme a versão
- `/healthz` permanece `200` durante a divergência: liveness e readiness são coisas diferentes
- O log nomeia a versão esperada e a encontrada
- O corpo do `503` identifica que a falha foi de schema, não de conexão

### C4 - Semeadura rapida e com manifesto

```bash
time make seed AUCTIONS=1000
docker compose exec -T postgres psql -U auction -d auction -tAc \
  'SELECT count(*), count(DISTINCT id) FROM auctions;'
jq 'length, .[0]' bench/auctions.json
make seed AUCTIONS=1 TRUNCATE=1
docker compose exec -T postgres psql -U auction -d auction -tAc \
  'SELECT count(*) FROM auctions;'
```

Aceite:

- `real` abaixo de 2 segundos para 1000 leilões
- A query devolve `1000|1000`
- `jq` reporta `1000` e um objeto com `id`, `version: 0` e `minNextBid: 100`
- Após `TRUNCATE=1`, a contagem é `1`

### C5 - Metricas do pool expostas e raspadas

```bash
curl -s localhost:8080/metrics | grep -E '^db_pool_'
curl -s 'localhost:9090/api/v1/query?query=db_pool_conns' | jq '.data.result | length'
curl -s 'localhost:9090/api/v1/targets' | jq '.data.activeTargets[].health'
```

Aceite:

- `/metrics` expõe `db_pool_conns` com os três labels de estado, mais `db_pool_acquire_total`, `db_pool_acquire_duration_seconds_total`, `db_pool_empty_acquire_total` e `db_pool_canceled_acquire_total`
- `db_pool_conns{state="max"}` é igual a `DB_POOL_SIZE`
- Prometheus retorna ao menos uma série e o target está `up`
- Nenhuma métrica `bid_*` aparece: elas chegam com a engine, na spec 02

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2 instalados
Portas livres: 5432, 6379, 8080, 9090, 3000
Repositorio limpo, .env criado a partir de .env.example
```

Passos:

```bash
make up && sleep 15
make seed AUCTIONS=3
curl -s localhost:8080/readyz | jq
curl -s localhost:8080/metrics | grep db_pool_conns
docker compose exec -T postgres psql -U auction -d auction -c \
  'SELECT id, highest_bid_cents, min_increment_cents, version, ends_at FROM auctions;'
open http://localhost:3000        # Grafana, datasource Prometheus configurado
make down
```

Aceite manual:

- `/readyz` responde `200` em menos de 15 segundos após `make up`, sem intervenção
- Os três leilões têm `version = 0`, `highest_bid_cents = 0`, `min_increment_cents = 100` e `ends_at` no futuro
- Grafana abre com o datasource Prometheus presente e testável, sem dashboards
- `make down` derruba tudo sem contêiner órfão

## Definicao De Pronto

- RF01 a RF07 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- Todos os testes de `## Testes` passando; `go vet ./...` limpo e `gofmt -l .` vazio
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado
- Nada de `BidEngine` ou de rota de lance no diff: a spec 02 começa do zero nesse ponto
