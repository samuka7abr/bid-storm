# Etapa 1 — Spec 03: Harness de carga e verificador de invariantes

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [spec 01](01-spec-fundacao.md) · [spec 02](02-spec-engine-otimista.md)

## Contexto

A spec 01 entregou a fundação e a spec 02 entregou o contrato HTTP inteiro da etapa 1 mais a primeira engine, provada sob concorrência real por `enginetest.RunConformance`. O que existe hoje funciona e está testado — e **nunca foi medido**.

Esta spec fecha a etapa 1 com as duas peças que a spec 02 adiou por nome (*"`cmd/checker` e o `handleSummary` do k6 — spec 03"*) e que o [roadmap](../../projeto/roadmap.md) aloca à etapa 1. Junto com elas vem a unidade que a etapa 5 vai repetir 36 vezes: **a célula**.

Quatro decisões da etapa 1 não têm uma linha de código até aqui, e todas moram no cliente: a **4** (a verdade sobre durabilidade vem do lado do cliente), a **5** (o apostador agressivo), a **13** (cada célula parte do mesmo estado) e a **18** (`MAX_RETRIES` e `BID_DEADLINE`). Enquanto elas forem só texto, o projeto tem uma engine que passa em teste e nenhuma evidência sob carga — que é a diferença entre demonstração e resultado, conforme [projeto/provas.md](../../projeto/provas.md).

Além dessas, sustentam as decisões abaixo a **7** (ambiente fixado e gravado), a **9** (`409` e `422` tratados igual pelo cliente), a **12** (`ends_at` como guarda), a **16** (`bid_attempts_per_accept` mora no k6 na etapa 1) e a **21** (`outcome="invalid"` denuncia k6 mal configurado).

## Objetivo

Transformar a engine da spec 02 em número, e o número em evidência que sobrevive a revisor hostil.

O sistema deve:

- Gerar carga com o apostador agressivo da decisão 5, com **contenção como variável independente** e o mesmo script servindo as três engines futuras
- Exportar do cliente o que só existe no cliente: quantos `201` ele recebeu e qual a maior `seq` que lhe foi confirmada
- Provar, depois de cada célula, os invariantes que precisam valer independentemente da estratégia, com saída não-zero que **barra** a execução
- Executar uma célula reprodutível ponta a ponta — reset, seed, warmup, reset, carga, verificação — gravando ambiente e resultado
- Fazer isso **sem tocar em uma linha de `internal/`**: se o harness exigir mudança no servidor, o contrato da spec 02 está errado e é isso que precisa ser reportado

## Fora de Escopo

- A matriz de 36 células, o sweep de pool e os dashboards do Grafana — etapa 5. Aqui a célula é o tijolo; a etapa 5 acrescenta o laço, não uma reescrita
- Engine pessimista e single-writer — etapas 2 e 3. O script já as atende por construção (decisão 9), mas `BID_STRATEGY` continua só aceitando `optimistic`
- Idempotência, `X-Idempotency-Key`, duplicatas injetadas no k6 e o invariante de chave repetida — etapa 2. O checker nasce com o lugar dele vazio e comentado
- `bid_attempts_per_accept` no servidor — etapa 2, quando a chave de idempotência der o fio que correlaciona três requisições como tentativas do mesmo lance (decisão 16)
- Cenários de caos e `closerd` — etapa 4
- Qualquer alteração em `internal/bid`, `internal/httpapi`, `internal/metrics`, `internal/store` ou `cmd/auctiond`

## Fluxo

```text
make bench RUN=<id> AUCTIONS=1 POLICY=immediate SCENARIO=ramp
  └── bench/run-cell.sh
        ├── psql  TRUNCATE bids, auctions RESTART IDENTITY CASCADE
        ├── make seed AUCTIONS=$N            → bench/auctions.json
        ├── psql  VACUUM ANALYZE
        ├── k6 -e WARMUP=1  (curto, descartado: aquece pool, cache e plano)
        ├── psql TRUNCATE + seed + VACUUM ANALYZE     ← o warmup escreveu; a
        │                                                célula medida não pode
        │                                                herdar as linhas dele
        ├── bench/env.sh                     → bench/results/$RUN/env.json
        ├── k6 run bench/bid-storm.js        → bench/results/$RUN/summary.json
        │     └── handleSummary              → bench/results/$RUN/client.json
        └── go run ./cmd/checker -run=$RUN   → bench/results/$RUN/checker.txt
              ├── I1 sequência densa por leilão                   SQL
              ├── I2 incremento respeitado na ordem de seq        SQL
              ├── I3 vencedor, versão e valor coerentes           SQL
              ├── I4 nenhum lance depois do fechamento            SQL
              ├── I5 durabilidade: db x cliente                   client.json
              └── I6 célula válida: invalid, erro, saturação      client.json + env.json
                    exit 0 ok · 1 invariante violado · 2 não deu para verificar
```

Dentro do k6, um lance lógico:

```text
pickAuction()  →  estado em cache (semeado pelo manifesto, atualizado por toda resposta)
  loop até MAX_RETRIES=10 ou BID_DEADLINE=2s
    POST {amountCents: cache.minNextBid, expectedVersion: cache.currentVersion}
      201 → bids_accepted++ · seq_seen.add(seq) · attempts_per_accept.add(i+1) · fim
      409 → bids_conflict++ · cache ← corpo · sleep(backoff(i)) · retenta
      422 → bids_outbid++   · cache ← corpo · sleep(backoff(i)) · retenta
      410 → bids_closed++   · terminal
      404/400/503 → bids_error++ / bids_invalid++ · terminal
  esgotou → bids_exhausted++
```

## Decisoes Tecnicas

### O servidor não muda, e isso é um requisito, não uma consequência

Nenhum arquivo de `internal/` ou de `cmd/auctiond` entra no diff desta spec.

A spec 02 publicou o contrato inteiro da etapa 1 sob a premissa de que ele bastaria para o benchmark: o estado no corpo de toda resposta existe para que a retentativa seja um `POST` só (decisão 2), `409` e `422` compartilham envelope para que a carga seja equivalente nas três engines (decisão 9), e `minNextBid` é do leilão para que dois VUs não compitam sob regras diferentes (decisão 17). Se o gerador de carga precisar de um campo novo, de uma rota nova ou de um comportamento diferente, então uma dessas três decisões está errada — e descobrir isso agora, com uma engine só, é barato.

Por isso a instrução ao implementador é parar e reportar em vez de emendar o servidor. Um `switch` a mais no handler para acomodar o k6 seria exatamente o tipo de acoplamento que [api.md](../../projeto/api.md) chama de condição do experimento.

### O eixo controlado é a concorrência oferecida, não o volume completado

[benchmark.md](../../projeto/benchmark.md) descreve a contenção como *"o mesmo volume total de requisições distribuído sobre 1, 10 ou 1000 leilões"*. Isso não é sustentável: com executores baseados em VUs, o volume completado **é o resultado** — sob contenção 1 o otimista completa menos requisições por segundo justamente porque colapsa. Fixar o volume exigiria estender a duração da célula até completá-lo, e a célula de contenção alta rodaria dez vezes mais tempo que a de contenção baixa, com dez vezes mais linhas na tabela ao final. Isso viola a decisão 13 por outro caminho.

O que fica fixo é a **carga oferecida**: mesmo perfil de VUs, mesma duração, mesmo apostador. O que varia entre células é `AUCTIONS`. `aceitos/s`, `p95` e `tentativas por aceito` são medidos, nunca controlados.

### O VU mantém um cache de estado por leilão

O manifesto de `cmd/seed` traz `version` e `minNextBid` de cada leilão. Se cada lance lógico partisse do manifesto, toda tentativa inicial depois do primeiro lance daquele leilão nasceria com versão velha — um `409` garantido por lance, constante, em todas as células. `bid_attempts_per_accept` ganharia um `+1` que é artefato do harness, e a comparação entre estratégias herdaria esse offset (a pessimista e o shard ignoram `expectedVersion`, então o `+1` apareceria só no otimista).

O VU guarda o último estado conhecido de cada leilão que tocou, semeado pelo manifesto e atualizado a partir do corpo de **toda** resposta que carrega estado — `201`, `409`, `422` e `410`. É o que um cliente real faz, é o que a decisão 2 desenhou o envelope para permitir, e deixa a amplificação de retentativa refletir contenção em vez de instrumentação.

O cache é por VU, não global: dois VUs disputando o mesmo leilão precisam poder estar desatualizados um em relação ao outro, senão não há disputa para medir.

### `409` e `422` são status esperados, e nenhum threshold aborta a célula

k6 conta como falha toda resposta com status `>= 400`. Sem intervenção, `http_req_failed` contabilizaria cada conflito como erro, o threshold `rate<0.01` de [benchmark.md](../../projeto/benchmark.md) reprovaria já na primeira dezena de requisições e o k6 sairia com código diferente de zero na célula que mais interessa.

`http.setResponseCallback(http.expectedStatuses(201, 409, 410, 422))` no contexto de init devolve a `http_req_failed` o significado que o threshold pressupõe: **erro real**. `404`, `400` e `503` continuam falha, e devem continuar — nenhum dos três é resposta esperada durante uma célula bem configurada.

O threshold de latência sai. `bid_confirm_latency: p(95)<200, p(99)<500` transformaria em reprovação exatamente o fenômeno que a tese prevê: o otimista sob 1000 VUs **deve** estourar esse p99, e é isso que o gráfico vai mostrar. Uma célula que falha por medir o que foi medida para medir é um harness com opinião sobre o resultado. A latência é gravada como dado; quem decide se o número é aceitável é a etapa 6, não o k6.

Sobra um threshold, e ele é sobre validade e não sobre desempenho: `http_req_failed: rate<0.01`. Acima disso a medição está quebrada, não a engine.

### A watermark do cliente é um `Trend`, e a densidade é provada em SQL

[provas.md](../../projeto/provas.md) esboça `new Gauge('max_seq_seen')` com tag por leilão. Isso não é implementável como escrito: o sink de `Gauge` do k6 exporta o **último** valor, não o máximo, e `handleSummary` não recebe quebra por tag — o que voltaria por leilão seria o que o último VU escreveu, não a maior `seq` vista.

`seq_seen` vira um `Trend`. O sink de `Trend` exporta `max` nativamente, agrega corretamente entre VUs, e custa uma amostra em memória por lance aceito — não um registro por requisição, que é o custo que a decisão 4 recusa.

A watermark passa a ser **global** (a maior `seq` que qualquer VU viu confirmada, em qualquer leilão), e isso continua provando o que precisa provar. Sob contenção 1 ela é literalmente a watermark daquele leilão. Sob 10 e 1000 leilões, a densidade por leilão — `count(*) = max(seq)` com `min(seq) = 1` — é provada em SQL pelo invariante I1, e densidade mais contagem total subsomem a atribuição por leilão: se um `201` durável sumiu de um leilão qualquer, ou a contagem do banco fica abaixo da do cliente (I5), ou a sequência daquele leilão ganha um buraco (I1). Não existe forma de perder um lance confirmado e passar nos dois.

### O contrato entre o k6 e o checker é um arquivo próprio

`handleSummary` escreve dois arquivos. `summary.json` é o `data` cru do k6, guardado como registro. `client.json` é um objeto pequeno, escrito à mão, com exatamente os campos que o checker consome.

O motivo é de acoplamento: o formato de `data` é interno do k6 e muda entre versões. Um checker que navega `data.metrics['http_req_duration'].values['p(95)']` quebra num upgrade de imagem, e quebra **calado** se um campo virar `null` em vez de sumir — o invariante de durabilidade passaria verde comparando contra zero. Com `client.json`, a quebra acontece no k6, na hora, e o checker falha com `2` (não deu para verificar) em vez de `0`.

```json
{
  "run": "20260819T143012", "strategy": "optimistic",
  "auctions": 1, "policy": "immediate", "scenario": "ramp",
  "accepted": 41870, "conflict": 918500, "outbid": 12044,
  "closed": 0, "invalid": 0, "error": 3, "exhausted": 4118,
  "attempts": 972414, "maxSeqSeen": 41870,
  "confirmLatencyMs": { "avg": 84.2, "p95": 310.5, "max": 1984.0 },
  "attemptsPerAccept": { "avg": 4.1, "p95": 9.0, "max": 10.0 }
}
```

`maxSeqSeen` e `accepted` são os dois números que o banco não pode desmentir. O resto é relatório.

### O warmup é descartado, e o que ele escreveu também

O laço de célula de [benchmark.md](../../projeto/benchmark.md) roda `k6 -e WARMUP=1`, depois a carga medida, sem reset no meio. Mas o warmup dá lances, e lances viram linhas: a célula medida começaria com a tabela já suja e as estatísticas já mexidas — precisamente o que a decisão 13 existe para impedir, só que dentro da célula em vez de entre células.

O reset se repete depois do warmup: `TRUNCATE`, `seed`, `VACUUM ANALYZE`. O que o warmup deixa para trás é justamente o que sobrevive ao `TRUNCATE` e é o motivo dele existir — conexões abertas no `pgxpool`, planos preparados por conexão, page cache do Postgres quente e o heap do processo Go estabilizado.

O `seed` do reset gera IDs novos, então o `setup()` do k6 relê `bench/auctions.json` na execução medida. Como cada `k6 run` é um processo novo, isso sai de graça.

### O gerador roda no compose, com limite próprio, e a saturação dele invalida a célula

Rodar `k6` no host, ao lado de contêineres com 2 CPUs cada, deixa o gerador de carga sem limite algum e sem registro no `env.json`. Pior: quando ele saturar, o efeito aparece como latência do servidor, porque a fila se forma no cliente e o relógio do cliente já está correndo.

`k6` entra no `docker-compose.yaml` sob `profiles: [bench]` — não sobe no `make up` — com limite próprio de CPU e memória, falando com `auctiond:8080` pela rede do compose. Isso também tira o port-forward do host do caminho quente.

Um limite só desloca o problema se ninguém olhar para ele. Por isso `run-cell.sh` amostra `docker stats` do contêiner do k6 durante a carga e grava o pico em `env.json`. Pico acima de 90% do limite significa que a célula pode ter medido o gerador, e o checker reporta isso como **WARN** com o número na cara — não como falha, porque a decisão de descartar a célula é de quem lê o relatório, mas nunca em silêncio.

### O checker é um binário Go, e ele não pergunta ao Prometheus

Quatro dos seis invariantes são SQL puro e caberiam num `.sql`. Os outros dois comparam banco com cliente, precisam de código de saída distinto e vão ganhar o invariante de idempotência na etapa 2 — e um script que faz metade em SQL e metade em Go é duas coisas para manter.

O checker **não** consulta o Prometheus, e isso é a decisão 4: comparar `bids` com `bid_outcomes_total{outcome="accepted"}` seria o `auctiond` conferindo a si mesmo. Se o handler mentir, a métrica mente junto e o invariante passa verde. A única fonte independente é o cliente, e é dela que o checker parte.

Uma consequência agradável da decisão 13: o banco contém exatamente uma célula quando o checker roda, então nenhuma query precisa de filtro por run. O escopo do checker é `SELECT ... FROM bids`, sem cláusula de recorte — e um recorte esquecido não pode esconder violação.

### Os invariantes que o checker prova

| # | Invariante | Fonte | Falha significa |
| --- | --- | --- | --- |
| I1 | Por leilão, `count(*) = max(seq)` e `min(seq) = 1` | SQL | Sequência com buraco: um `INSERT` aceito sumiu, ou a engine pulou posição |
| I2 | Na ordem de `seq`, `amount_cents >= lag(amount_cents) + min_increment_cents` | SQL | A engine aceitou lance que não cumpria o incremento — o alvo é o shard da etapa 3 |
| I3 | `auctions.highest_bid_cents = max(bids.amount_cents)`, `auctions.version = count(bids)` e `highest_bidder` = usuário do maior `seq` | SQL | O estado publicado diverge da história gravada |
| I4 | Nenhum `bids.created_at > auctions.ends_at` | SQL | Entrou lance depois do fechamento (decisão 12) |
| I5 | `count(bids) >= client.accepted` e `max(seq) >= client.maxSeqSeen` | client.json | Um `201` durável desapareceu |
| I6 | `client.invalid = 0`, `client.error / total < 1%`, gerador não saturado | client.json + env.json | A célula não vale: k6 mal configurado, infra caindo ou gerador no limite |

I2 é mais forte que o esboço de [provas.md](../../projeto/provas.md), que verifica só monotonicidade. Valor estritamente crescente é o que o `UNIQUE` e o `WHERE` do otimista já garantem; a regra do incremento é o que o shard, decidindo em memória sem `WHERE` nenhum, pode violar sem deixar buraco na sequência (decisão 24). O invariante nasce agora exatamente para estar de pé antes da engine que ele existe para pegar.

I5 tem duas severidades, e a assimetria é da decisão 4. `count(bids) < client.accepted` é **FAIL**: um lance confirmado ao cliente não está no banco. `count(bids) > client.accepted` é **WARN**: na etapa 1, sem idempotência, um `201` cuja resposta não chegou ao cliente é divergência legítima. Na etapa 2 essa tolerância cai.

I4 não é vacuoso, e vale a pena registrar por quê: o `UPDATE` do otimista guarda o aceite com `now() < ends_at`, e `bids.created_at` tem `DEFAULT now()`. Dentro da mesma CTE, os dois `now()` são o mesmo `transaction_timestamp()`, então um lance aceito tem `created_at < ends_at` por construção — o invariante compara o que o banco decidiu com o que o banco gravou, usando um relógio só (decisão 22).

### Observabilidade

Nenhuma série nova no servidor. A decisão 16 aloca `bid_attempts_per_accept` ao k6 nesta etapa, e é lá que ela fica, como `Trend`:

| Métrica do cliente | Tipo | Para quê |
| --- | --- | --- |
| `bids_accepted`, `bids_conflict`, `bids_outbid`, `bids_closed`, `bids_invalid`, `bids_error` | Counter | Um por desfecho; o espelho, do lado do cliente, de `bid_outcomes_total` |
| `bids_exhausted` | Counter | O colapso do otimista tornado visível (decisão 18). Manchete, não rodapé |
| `bids_attempts` | Counter | Denominador da amplificação, incluindo os lances que desistiram |
| `bid_attempts_per_accept` | Trend | Amplificação de retentativa, distribuição e não média (decisão 16) |
| `bid_confirm_latency` | Trend | Latência até `201` **vista pelo cliente**, em ms |
| `seq_seen` | Trend | `max` é a watermark que alimenta I5 |

`bid_confirm_latency` é o gêmeo cliente de `bid_confirm_duration_seconds{strategy}`, que a spec 02 já expõe no servidor. Ter os dois é o que separa *"o servidor está lento"* de *"o cliente está na fila"*: a diferença entre as duas curvas é enfileiramento antes do handler, e sob 1000 VUs ela é grande o suficiente para mudar a leitura do gráfico.

## Requisitos Funcionais

### RF01 - `bench/bid-storm.js`, o apostador

Contexto de init: lê `bench/auctions.json` via `open()` dentro de um `SharedArray`, e chama `http.setResponseCallback(http.expectedStatuses(201, 409, 410, 422))`.

Identidade: cada VU tem um `X-User-Id` estável, derivado de `__VU` como UUID determinístico (`00000000-0000-4000-8000-<vu>` com zeros à esquerda). Determinístico porque `highest_bidder` no banco passa a ser rastreável até o VU, sem nenhum registro por requisição.

Ciclo, exatamente como o Fluxo: escolha uniforme entre os `AUCTIONS` leilões, cache de estado por leilão semeado pelo manifesto, laço limitado por `MAX_RETRIES = 10` **e** `BID_DEADLINE = 2000ms`, o que vier primeiro (decisão 18). `amountCents` é sempre o `minNextBid` do cache e `expectedVersion` é sempre o `currentVersion` do cache — o servidor define o incremento (decisão 17), o cliente nunca inventa valor.

`409` e `422` são tratados de forma idêntica na lógica e distinta apenas no contador (decisão 9). `410`, `404`, `400` e `503` são terminais.

`RETRY_POLICY=immediate|jitter`, lido de `__ENV`. `immediate` devolve 0; `jitter` devolve `Math.random() * Math.min(200, 5 * 2 ** attempt)`, o *full jitter* de [benchmark.md](../../projeto/benchmark.md).

### RF02 - Métricas do cliente

As seis contagens por desfecho, `bids_exhausted`, `bids_attempts` e os três `Trend` da tabela de Observabilidade, com esses nomes.

`bid_attempts_per_accept` recebe amostra **apenas** quando o lance é aceito, com o número de tentativas daquele lance lógico. Lances que esgotaram não entram: a métrica se chama *por aceito*, e misturar nela quem desistiu produziria um número que não é nem amplificação nem taxa de desistência. Quem desistiu está em `bids_exhausted`, e `bids_attempts` guarda o denominador bruto para quem quiser as duas leituras.

### RF03 - Cenários e opções

`SCENARIO` seleciona um dos três, e apenas um roda por execução:

- `smoke` — poucos VUs, poucos segundos. Existe para validar o harness e os checkpoints sem esperar dois minutos e meio
- `ramp` — `ramping-vus`, `30s→100`, `1m→500`, `30s→0`
- `last_second_spike` — `constant-vus`, 1000 VUs, 15s

`WARMUP=1` encurta o cenário selecionado para uma fração e desliga os thresholds: o warmup não pode reprovar nada, ele só esquenta.

Threshold único: `http_req_failed: ['rate<0.01']`. Nenhum threshold de latência.

`ENDS_IN` do seed é folgado o bastante para que nenhum leilão feche durante a célula. Uma célula em que os leilões morrem no meio mistura dois mecanismos — contenção e a borda do fechamento — num número só; a borda merece célula própria, e ela é da etapa 5. `bids_closed > 0` numa célula desta etapa é sinal de `ENDS_IN` curto demais, e o checker avisa.

### RF04 - `handleSummary` e o contrato com o checker

`handleSummary(data)` devolve dois arquivos, com os caminhos derivados de `__ENV.RUN`:

- `bench/results/<RUN>/summary.json` — `JSON.stringify(data)`, o registro cru
- `bench/results/<RUN>/client.json` — o objeto do exemplo em Decisões Técnicas, montado campo a campo

Campo ausente em `data` vira erro em vez de `null`: `client.json` que não pode ser montado por completo faz o `handleSummary` lançar, e a célula falha no k6 e não três passos adiante.

### RF05 - `cmd/checker`

`go run ./cmd/checker -run=<id>` lê `DATABASE_URL` do ambiente e `bench/results/<id>/{client.json,env.json}` do disco.

Executa I1 a I6 na ordem da tabela, sempre todos — um invariante que falha não interrompe os outros, porque o relatório da célula precisa dizer tudo o que está errado de uma vez.

Saída em texto, uma linha por invariante, com veredito e número:

```text
I1  sequência densa por leilão          OK    1 leilão, 41870 lances
I2  incremento respeitado               OK
I3  vencedor e versão coerentes         OK
I4  nenhum lance após o fechamento      OK
I5  durabilidade db x cliente           WARN  db=41871 cliente=41870 (+1 resposta não entregue)
I6  célula válida                       OK    invalid=0 erro=0.007% gerador=61% do limite
resultado: OK com 1 aviso
```

Códigos de saída: `0` verde, com ou sem avisos; `1` invariante violado; `2` não deu para verificar (banco fora, arquivo ausente ou ilegível). A distinção entre `1` e `2` existe para o laço da etapa 5: os dois barram a matriz, mas só um deles é resultado.

`-json` escreve o mesmo relatório em JSON ao lado do texto, para a etapa 5 agregar sem parsear tabela.

### RF06 - `bench/run-cell.sh`

Executa a sequência do Fluxo, com `set -euo pipefail`, parando no primeiro erro. Parâmetros por variável de ambiente com default: `RUN` (timestamp), `AUCTIONS` (1), `POLICY` (`immediate`), `SCENARIO` (`smoke`), `STRATEGY` (`optimistic`).

Antes de qualquer coisa: verifica que o `auctiond` no ar está rodando `STRATEGY` e responde `200` em `/readyz`. Uma célula rodada contra a estratégia errada produz uma linha de resultado plausível e falsa, e é o erro mais caro possível numa matriz de 36 — porque não deixa rastro.

Amostra `docker stats --no-stream` do contêiner do k6 durante a carga e repassa o pico ao `env.json`.

Ao final, invoca o checker e propaga o código de saída dele.

### RF07 - `bench/env.sh` e `env.json`

Grava, em `bench/results/<RUN>/env.json`: `run`, `startedAt`, `finishedAt`, commit e estado sujo do git, a célula (`strategy`, `auctions`, `policy`, `scenario`, `poolSize`), o host (kernel, CPUs, memória), as imagens com tag, as versões efetivas de Postgres, Go e k6, e os limites de CPU e memória **lidos de `docker inspect`**, não do YAML — pelo mesmo motivo do C1 da spec 01: um limite declarado que o Compose ignorou não é um limite, e o `env.json` de um número publicado não pode mentir sobre isso.

Inclui `generator.cpuPctPeak` e `generator.saturated`.

### RF08 - Compose e Makefile

`docker-compose.yaml` ganha o serviço `k6` sob `profiles: [bench]`, com a imagem fixada, `./bench` e `./bench/results` montados, limite de CPU e memória próprios, e `BASE_URL=http://auctiond:8080`.

`Makefile` ganha `bench` (repassa `RUN`, `AUCTIONS`, `POLICY`, `SCENARIO`, `STRATEGY` ao `run-cell.sh`) e `check` (`RUN`), ambos usando `HOST_DB_URL` como os alvos existentes. `.gitignore` ganha `/bench/results/`.

## Requisitos Nao Funcionais

- Nenhuma dependência Go nova: `pgx/v5` e a biblioteca padrão bastam para o checker
- Nenhum arquivo de `internal/` ou de `cmd/auctiond` no diff. Se parecer necessário, pare e reporte
- O script do k6 não ramifica por estratégia em lugar nenhum: o mesmo arquivo roda as três, sem `if strategy`
- `go vet ./...` limpo; `gofmt -l .` vazio; `k6 archive bench/bid-storm.js` sem erro
- `bench/run-cell.sh` é idempotente: rodar duas vezes seguidas com o mesmo `RUN` sobrescreve os artefatos e produz o mesmo veredito
- O checker completo roda em menos de 5 segundos sobre uma célula de ~50k lances
- A célula `SCENARIO=smoke` completa em menos de 60 segundos, reset e verificação incluídos
- Nenhuma credencial literal em arquivo versionado; os scripts leem `.env` como o `Makefile` já faz

## Budget do PR

Até 15 arquivos e aproximadamente 750 linhas de código próprio, JS e shell incluídos.

Se passar disso, o corte provável é separar `cmd/checker` (com seus testes) do harness k6 em dois PRs — nessa ordem, porque o checker é testável sem carga e o harness não é verificável sem o checker.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: medio
- Referencia permitida: `docs/projeto/provas.md`, `docs/projeto/benchmark.md`, `docs/projeto/api.md`, `docs/projeto/schema.md`, `docs/projeto/observabilidade.md`, `docs/decisoes/etapa-1.md`, `docs/specs/etapa-1/01-spec-fundacao.md`, `docs/specs/etapa-1/02-spec-engine-otimista.md`, `docs/specs/etapa-1/03-spec-carga-e-checker.md`

Prompt:

```text
Implemente docs/specs/etapa-1/03-spec-carga-e-checker.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-1/03-spec-carga-e-checker.md  (a spec — a autoridade)
  docs/projeto/provas.md                         (os invariantes)
  docs/projeto/benchmark.md                      (o modelo do apostador)
  docs/projeto/api.md                            (o contrato ja publicado)
  docs/decisoes/etapa-1.md                       (o porque; decisoes 4, 5, 7,
                                                  9, 12, 13, 16, 18, 21, 22, 24)

ATENCAO: a spec emenda provas.md e benchmark.md em sete pontos, todos
argumentados na secao Decisoes Tecnicas. Onde os dois discordarem, a spec vence:
  - o eixo controlado e a carga oferecida, nao o volume completado
  - o VU mantem cache de estado por leilao; o manifesto so semeia a 1a tentativa
  - 409/422 sao expectedStatuses; nenhum threshold de latencia
  - a watermark e um Trend global (seq_seen), nao um Gauge com tag por leilao
  - o contrato k6 -> checker e client.json, nao o data cru do k6
  - o warmup e seguido de reset completo (truncate + seed + vacuum)
  - o k6 roda no compose, com limite proprio, e a saturacao dele e gravada

Escopo: apenas RF01..RF08. NAO implemente a matriz de 36 celulas, sweep de
pool, dashboards, engine pessimista, engine shard, idempotencia nem caos.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere nada em internal/ nem em cmd/auctiond. Se o harness parecer
  exigir mudanca no servidor, PARE e reporte: significa que o contrato da
  spec 02 esta errado, e isso e mais importante que esta spec.
- O checker nao consulta o Prometheus em hipotese nenhuma (decisao 4).
- Rode os checkpoints C1..C5 e cole a saida real de cada um. Nao declare
  aceite sem a saida do comando.
- Se estourar o budget de 15 arquivos / ~750 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
bench/bid-storm.js
bench/run-cell.sh
bench/env.sh
cmd/checker/main.go
cmd/checker/invariants.go
cmd/checker/client.go
```

Editar:

```text
docker-compose.yaml   (servico k6 sob profiles: [bench])
Makefile              (alvos bench e check)
.gitignore            (/bench/results/)
```

`cmd/checker/client.go` existe separado de `invariants.go` porque I5 e I6 são a única parte do checker que depende de um arquivo escrito por outro processo. Quando a etapa 2 acrescentar o invariante de idempotência — que é SQL puro — ele entra em `invariants.go` e `client.go` não muda.

## Testes

Adicionar:

```text
cmd/checker/invariants_test.go   cada invariante SQL: banco verde e violacao plantada
cmd/checker/client_test.go       I5 e I6 sobre client.json/env.json de fixture
```

`invariants_test.go` usa `internal/testsupport`, que a spec 01 criou justamente para ser reusado. Cada invariante tem dois casos: um banco coerente (verde) e um banco com a violação **plantada por SQL direto** — apagar um lance do meio para abrir buraco em I1, baixar um `amount_cents` para violar I2, mexer em `auctions.highest_bid_cents` para violar I3, empurrar `created_at` para depois de `ends_at` em I4.

O caso da violação plantada é o que importa mais que o caso verde. Um checker que só foi testado contra banco correto pode estar verde por engano de query — um `JOIN` errado devolve zero linhas, e zero linhas é exatamente o que ele lê como "invariante respeitado". Sem o teste de violação, o projeto inteiro passaria a confiar num verificador que nunca reprovou nada.

`client_test.go` cobre as três leituras de I5 (`<` falha, `>` avisa, `=` passa), `invalid > 0`, a taxa de erro no limite de 1%, e `client.json` truncado ou com campo faltando resultando em código de saída `2`.

O script do k6 não ganha teste unitário: não há runtime JS no CI, e o que ele tem de arriscado — o mapeamento de status para contador — é verificado pelo C2, que compara três fontes independentes.

## Checkpoints Mensuraveis

### C1 - Uma celula completa, verde ponta a ponta

```bash
make up && sleep 15
make bench RUN=c1 AUCTIONS=1 SCENARIO=smoke POLICY=immediate
echo "exit=$?"
ls bench/results/c1/
jq '{accepted, exhausted, maxSeqSeen, invalid, error}' bench/results/c1/client.json
cat bench/results/c1/checker.txt
```

Aceite:

- `exit=0`
- `bench/results/c1/` contém `env.json`, `summary.json`, `client.json` e `checker.txt`
- `I1` a `I6` aparecem no relatório, todos `OK` (ou `I5` com `WARN`), e a última linha diz `resultado: OK`
- `invalid` é `0`: `outcome="invalid"` acima de zero denunciaria k6 mandando lance sem `expectedVersion` (decisão 21)
- `accepted` maior que zero e `maxSeqSeen` igual a `accepted` — com um leilão só, a watermark **é** a contagem

### C2 - Cliente, servidor e banco concordam

```bash
jq -r '.accepted' bench/results/c1/client.json
curl -s localhost:8080/metrics \
  | grep '^bid_outcomes_total{.*outcome="accepted"' 
docker compose exec -T postgres psql -U auction -d auction -tAc \
  'SELECT count(*), max(seq) FROM bids;'
```

Aceite:

- Os três números são iguais: `client.accepted` == `bid_outcomes_total{outcome="accepted"}` == `count(*)` de `bids`
- `max(seq)` é igual a `count(*)`: a sequência é densa, sem buraco
- Divergência entre o cliente e os outros dois reprova o checkpoint — é write perdido ou contador errado, e os dois precisam ser investigados antes de qualquer número ser publicado

Este checkpoint usa o Prometheus como **diagnóstico**, não como fonte de verdade. O checker continua proibido de consultá-lo (decisão 4): aqui a comparação vale porque as três fontes são exibidas lado a lado para um humano, não porque uma confere a outra.

### C3 - O checker reprova violacao plantada

```bash
make bench RUN=c3 AUCTIONS=1 SCENARIO=smoke

# I1 — buraco na sequencia
docker compose exec -T postgres psql -U auction -d auction -c \
  "DELETE FROM bids WHERE seq = (SELECT max(seq) - 1 FROM bids);"
make check RUN=c3; echo "exit=$?"

# I2 — incremento violado
docker compose exec -T postgres psql -U auction -d auction -c \
  "UPDATE bids SET amount_cents = 1 WHERE seq = (SELECT max(seq) FROM bids);"
make check RUN=c3; echo "exit=$?"

# I4 — lance depois do fechamento
docker compose exec -T postgres psql -U auction -d auction -c \
  "UPDATE bids SET created_at = created_at + interval '1 year' WHERE seq = 1;"
make check RUN=c3; echo "exit=$?"

# I5 — lance confirmado que sumiu
docker compose exec -T postgres psql -U auction -d auction -c "DELETE FROM bids;"
make check RUN=c3; echo "exit=$?"
```

Aceite:

- Os quatro `make check` saem com `exit=1`
- Cada um nomeia o invariante violado e imprime o número que o denunciou
- Nenhum deles sai com `0`: um checker que não reprova violação plantada é um checker que nunca reprovou nada, e todo o resto do projeto passaria a confiar nele

### C4 - Contencao muda a curva

```bash
make bench RUN=c4-alta  AUCTIONS=1    SCENARIO=ramp POLICY=immediate
make bench RUN=c4-baixa AUCTIONS=1000 SCENARIO=ramp POLICY=immediate
for r in c4-alta c4-baixa; do
  jq -r --arg r $r '[$r, .accepted, .conflict, .exhausted,
                     .attemptsPerAccept.avg, .confirmLatencyMs.p95] | @tsv' \
     bench/results/$r/client.json
done
jq -r '.generator | "gerador: \(.cpuPctPeak)% saturado=\(.saturated)"' \
   bench/results/c4-alta/env.json
```

Aceite:

- As duas células saem `OK` no checker
- `attemptsPerAccept.avg` é claramente maior em `c4-alta` que em `c4-baixa`, e `conflict` também: é a prova de que `AUCTIONS` funciona como eixo do experimento, sem a qual a matriz da etapa 5 não mede nada
- `generator.saturated` é `false` nas duas. `true` invalida a célula: o número seria do k6, não do `auctiond`
- `invalid` é `0` nas duas

### C5 - Politica de retentativa e ambiente gravado

```bash
make bench RUN=c5-imm AUCTIONS=1 SCENARIO=ramp POLICY=immediate
make bench RUN=c5-jit AUCTIONS=1 SCENARIO=ramp POLICY=jitter
for r in c5-imm c5-jit; do
  jq -r --arg r $r '[$r, .policy, .accepted, .exhausted,
                     .attemptsPerAccept.p95, .confirmLatencyMs.p95] | @tsv' \
     bench/results/$r/client.json
done
jq '{git, cell, limits, versions}' bench/results/c5-jit/env.json
docker compose exec -T postgres psql -U auction -d auction -tAc \
  'SELECT count(*) FROM bids;'
```

Aceite:

- As duas células saem `OK`, e `client.json` reporta a `policy` correta em cada uma
- Os números diferem entre as duas: `RETRY_POLICY` é eixo do experimento (decisão 6), e duas células idênticas significariam que o backoff não está sendo aplicado
- `env.json` traz commit, `dirty`, os limites de CPU e memória vindos do `docker inspect` — nenhum deles `0` — e as versões efetivas de Postgres, Go e k6
- A contagem de `bids` no banco bate com `accepted` da **última** célula, e não com a soma das duas: o reset entre células funcionou (decisão 13)

## Smoke Manual

Pre-condicoes:

```text
Docker e docker compose v2, jq e make instalados
Portas livres: 5432, 6379, 8080, 9090, 3000
.env criado a partir de .env.example, arvore limpa
```

Passos:

```bash
make up && sleep 15
make bench RUN=manual AUCTIONS=1 SCENARIO=smoke
cat bench/results/manual/checker.txt
docker compose exec -T postgres psql -U auction -d auction -c \
  'SELECT seq, amount_cents, user_id FROM bids ORDER BY seq DESC LIMIT 5;'
open http://localhost:9090/graph        # bid_outcomes_total, bid_confirm_duration_seconds
make down
```

Aceite manual:

- A célula roda sem intervenção e o relatório sai legível, cabendo numa tela
- Os cinco últimos lances mostram `amount_cents` subindo de `min_increment_cents` em `min_increment_cents`, e `user_id` no formato determinístico por VU
- No Prometheus, `bid_outcomes_total` tem série para `accepted` e `conflict`, e nenhuma para `invalid`
- `make down` derruba tudo, incluindo o contêiner do k6, sem órfão

## Definicao De Pronto

- RF01 a RF08 implementados
- C1 a C5 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- Testes de `## Testes` passando, incluindo os quatro casos de violação plantada; `go vet ./...` limpo e `gofmt -l .` vazio
- `git diff --stat` não mostra nenhum arquivo em `internal/` nem em `cmd/auctiond`
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado
- A etapa 1 fecha aqui: schema, compose, API, engine otimista, suíte de conformidade, `cmd/seed` e `cmd/checker` entregues. A etapa 2 começa com a engine pessimista passando na suíte que já existe

---

## Emendas desta spec

Sete decisões tomadas ao desenhar esta spec emendam ou fortalecem o que está publicado em `projeto/benchmark.md` e `projeto/provas.md`. Estão argumentadas em Decisões Técnicas e ficam **a registrar como decisões 25 a 31** em [decisoes/etapa-1.md](../../decisoes/etapa-1.md), no mesmo padrão das emendas 19 a 24 da spec 02:

| # | Emenda | Emenda o quê |
| --- | --- | --- |
| 25 | O eixo controlado é a carga oferecida, não o volume completado | `benchmark.md`, *"o mesmo volume total de requisições"* |
| 26 | O VU mantém cache de estado por leilão | `benchmark.md`, o `a.next()` do esboço |
| 27 | `409`/`422` são `expectedStatuses`; nenhum threshold de latência barra a célula | `benchmark.md`, o bloco `thresholds` |
| 28 | A watermark do cliente é um `Trend` global; a densidade por leilão é provada em SQL | `provas.md`, `new Gauge('max_seq_seen')`, e o texto da decisão 4 |
| 29 | O contrato entre k6 e checker é `client.json` | `provas.md`, o `handleSummary` do esboço |
| 30 | O warmup é seguido de reset completo | `benchmark.md`, o laço de célula |
| 31 | O gerador roda no compose, com limite próprio e saturação gravada | `benchmark.md` e `arquitetura.md#ambiente-fixado` |

A 28 é a que mais muda algo já decidido, e vale dizer por quê em uma linha: a decisão 4 pede a watermark por leilão, e o k6 não tem primitivo que exporte máximo por tag. O invariante que ela protege continua de pé porque densidade mais contagem total provam a mesma coisa — e isso é uma troca de mecanismo, não de garantia.
