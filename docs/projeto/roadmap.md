# Roadmap e escopo

[← índice](../README.md)

## Etapas

| Etapa | Entrega |
| --- | --- |
| **1** | Schema e migrations, `docker compose` com recursos fixados, API Gin, engine otimista com classificação de desfecho, suíte de conformidade, `cmd/seed`, `cmd/checker` |
| 2 | Engine pessimista, middleware de idempotência, duplicatas injetadas no k6, `bid_attempts_per_accept` migra do k6 para o servidor |
| 3 | Engine single-writer com shards e commit em lote, métricas de profundidade e de lote |
| 4 | Fechamento via Redis Streams, `closerd` materializando `status`, cenários de caos |
| 5 | Matriz de 36 células, sweep de pool do pessimista, dashboards Grafana |
| 5b | Tick agregado no WebSocket, geração de tipos, painel React de três colunas |
| 6 | Escrita dos resultados, gráfico de cruzamento, README final |

Cada etapa é quebrada em specs do tamanho de um PR, em `docs/specs/etapa-n/`:

| Spec | Entrega |
| --- | --- |
| [etapa-1/01-spec-fundacao.md](../specs/etapa-1/01-spec-fundacao.md) | Compose com recursos verificados, migration `001`, pool, `/readyz` com fail-fast de schema, `cmd/seed` |

As decisões de design da etapa 1 estão em [decisoes/etapa-1.md](../decisoes/etapa-1.md). Várias delas foram tomadas cedo de propósito: o contrato de durabilidade e o envelope uniforme de resposta precisam existir antes da primeira engine, ou as etapas 2 e 3 quebrariam a API que a etapa 1 publicou.

---

## Fora de escopo

Registrado de propósito, porque saber o que não fazer é parte do desenho.

| Não tem | Por quê |
| --- | --- |
| Múltiplas linguagens | Escolher três linguagens para seis serviços adiciona manutenção, não capacidade. A comparação entre estratégias exige a mesma linguagem nas três, ou o benchmark não significa nada |
| API Gateway, Auth Service | Não mudam nenhuma curva do gráfico. Identidade é `X-User-Id` no header; quando virar JWT, é um middleware e nada abaixo dele muda |
| gRPC | Otimização de transporte para um problema que não é de transporte |
| Kubernetes, Terraform, EKS | Custo real e semanas de trabalho para não alterar nenhum resultado. Orquestração é assunto de outro projeto |
| SNS/SQS | Redis Streams entrega as mesmas garantias localmente e deixa o mecanismo de retentativa visível |
| Microserviços | Dois processos, e o segundo existe apenas porque precisa ser morto no teste de caos |
| Catálogo, carrinho, pagamento | Leilão aqui é carga de trabalho, não produto |

**Regra de escopo:** uma pergunta, três implementações, um benchmark. Toda ideia nova passa pelo filtro "isso muda o gráfico?". Se não muda, fica de fora.
