# Observabilidade

[← índice](../README.md)

Cada processo expõe `/metrics`. Uma métrica só entra quando existe engine para alimentá-la — declarar as sete na etapa 1 produziria um dashboard com cinco painéis vazios e nenhuma forma de saber se estão vazios por design ou por bug.

| Métrica | Tipo | Entra na etapa | Para quê |
| --- | --- | --- | --- |
| `bid_confirm_duration_seconds{strategy}` | Histogram | 1 | Latência até **durável**, fim a fim. É o eixo central da comparação |
| `bid_outcomes_total{strategy,outcome}` | Counter | 1 | Um contador para `accepted`/`conflict`/`too_low`/`closed`/`not_found`. Conflito vira filtro por label |
| `db_pool_empty_acquire_total` | Counter | 1 | Aquisições que esperaram porque o pool estava vazio. Prova que a fila **não** se formou no `pgxpool` — sem isso, o custo do pessimista é indistinguível de pool subdimensionado |
| `db_pool_acquire_total` e `db_pool_acquire_duration_seconds_total` | Counter | 1 | A razão entre os dois `rate()` dá a espera média por aquisição |
| `db_pool_conns{state}` | Gauge | 1 | Conexões em uso, ociosas e o teto configurado |
| `idempotency_hits_total` | Counter | 2 | Duplicatas efetivamente barradas |
| `bid_attempts_per_accept` | Histogram | 2 | Amplificação de retentativa |
| `lock_wait_duration_seconds` | Histogram | 2 | Espera em `FOR UPDATE` |
| `bid_accept_duration_seconds{strategy}` | Histogram | 3 | Aceite decidido em memória. O gap para `confirm` é o custo da durabilidade |
| `shard_inbox_depth{shard}` | Gauge | 3 | Backpressure no modo single-writer |
| `shard_batch_size` | Histogram | 3 | Quantos lances por `fsync` — é daqui que vem o ganho do shard |
| `journal_lag_seconds` | Gauge | 3 | Distância entre decidido e persistido |
| `stream_pending_entries` | Gauge | 4 | Mensagens não confirmadas no fechamento |

---

## Duas notas que mudam o desenho

**`bid_conflicts_total` não existe como métrica própria.** Ele é `bid_outcomes_total{outcome="conflict"}`. Um contador com label evita o erro de contar `409` e `422` em séries separadas e depois esquecer de somá-las ao comparar estratégias que produzem um mas não o outro.

**As métricas do pool são contadores, não histograma.** `pgxpool` não expõe a duração de cada aquisição individualmente — `Stat()` devolve acumulados. Um histograma exigiria embrulhar toda aquisição no código da aplicação, o que adicionaria custo no caminho quente para medir o caminho quente. Os contadores respondem a mesma pergunta: `db_pool_empty_acquire_total` cresce se e somente se alguém esperou por conexão.

**`bid_attempts_per_accept` mora no k6 na etapa 1.** O servidor não tem como saber que três requisições distintas eram tentativas do mesmo lance — não há nada que as correlacione. A partir da etapa 2, a chave de idempotência dá esse fio, e a métrica migra do cliente para o servidor. Até lá, o número vem do gerador de carga, e o documento registra a diferença em vez de fingir que é a mesma medição.

---

## Dashboard

Único, provisionado por arquivo em `deploy/grafana/`, com painel comparativo lado a lado das três estratégias. Painéis obrigatórios:

- Throughput de aceitos por nível de contenção, uma série por estratégia
- `bid_confirm_duration_seconds` p95 e p99, uma série por estratégia
- Distribuição de `bid_outcomes_total` por `outcome`, empilhada
- `db_pool_acquire_duration_seconds` — a prova de que o gargalo não é o pool
- `shard_batch_size` contra `bid_confirm_duration_seconds` — o trade-off do lote, visível
