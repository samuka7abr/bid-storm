# Decisões — Etapa 1

[← índice](../README.md)

Registro das decisões de design tomadas antes da primeira linha de código, com o porquê de cada uma. Várias tratam de etapas futuras: foram antecipadas porque definem contratos que a etapa 1 publica e que quebrariam se fossem revistos depois.

---

### 1. `RowsAffected() == 0` é classificado, não colapsado em `ErrConflict`

No caminho de falha, um `SELECT` do estado atual separa `Conflict`, `TooLow`, `Closed` e `NotFound`.

**Por quê:** o `UPDATE` condicional falha por quatro razões incompatíveis. Tratar as quatro como conflito faria um lance de R$ 5 num leilão de R$ 9.000 contar como conflito de concorrência — `bid_outcomes_total{outcome="conflict"}` e "tentativas por aceito", que são as métricas centrais da tese, virariam ruído. E o cliente retentaria para sempre um lance que nunca passaria.

**Custo:** um round-trip a mais, apenas no caminho de falha. O caminho feliz não paga nada.

---

### 2. Toda resposta carrega o estado atual do leilão

`201`, `409` e `422` devolvem `currentVersion`, `currentHighestBid` e `minNextBid`.

**Por quê:** sem isso o cliente precisaria de um `GET` antes de cada tentativa, e o benchmark passaria a medir leitura misturada com escrita. Com o estado no corpo, a retentativa é um único `POST`.

---

### 3. `bids` guarda só aceitos, com `UNIQUE (auction_id, seq)`

`INSERT` na mesma transação do `UPDATE`. Rejeições viram métrica, não linha.

**Por quê:** persistir tentativas rejeitadas faria o modo otimista escrever ~20x mais que os outros sob alta contenção, por um motivo que **não é** controle de concorrência — o benchmark mentiria a favor da própria hipótese.

`seq` é a `version` resultante. O `UNIQUE` transforma "sequência estritamente crescente" de invariante *detectável* em invariante *impossível de violar*: se alguma engine tiver uma corrida, o `INSERT` explode em vez de gravar história inconsistente. Ordenar por `created_at` não serviria — sob 1000 VUs, timestamps empatam.

---

### 4. O checker consome a verdade do lado do cliente

`handleSummary` do k6 exporta contadores por leilão e a maior `seq` vista num `201`. O `cmd/checker` compara com o banco.

**Por quê:** o invariante *"todo lance que recebeu 201 está no banco"* depende de um número que só existe no cliente. Comparar contra a métrica Prometheus do `auctiond` seria o servidor conferindo a si mesmo: se o handler mentir, a métrica mente junto.

`db < cliente` falha. `db > cliente` é aviso, não falha — na etapa 1, sem idempotência, um `201` cuja resposta não chegou é divergência legítima. A watermark de `seq` pega write perdido ao custo de um gauge, sem registro por requisição.

---

### 5. O apostador do k6 é agressivo

A cada rejeição, o VU re-mira: `amount = minNextBid` devolvido pela resposta.

**Por quê:** com valor fixo, todo mundo levaria `422` depois da primeira rodada, a taxa de conflito iria a zero e o otimista apareceria como ótimo. A hipótese seria refutada por acidente de instrumentação, não por evidência.

---

### 6. `RETRY_POLICY=immediate|jitter` é eixo do experimento

Implementado na etapa 1, rodado nos dois valores na etapa 5.

**Por quê:** a crítica mais forte ao projeto é *"seu otimista colapsou porque você não usou backoff"*. É válida — retry imediato sob 1000 VUs produz colapso por congestionamento, e o gráfico estaria medindo a política de retentativa. Custa uma variável de ambiente e responde a crítica com dado. Se o backoff salvar o otimista, isso falsifica a hipótese, o que é resultado.

**Consequência:** a matriz vai de 18 para 36 células.

---

### 7. Pool idêntico nas três engines, recursos limitados, ambiente gravado

`DB_POOL_SIZE` é constante durante a comparação; CPU e memória têm limite no compose; cada run grava `env.json`.

**Por quê:** o pool **é** a história do pessimista. Com pool pequeno, a fila se forma no `pgxpool` e não no lock do Postgres, e o custo de configuração seria atribuído ao mecanismo. Sem limites de recurso, o número vale só para uma máquina num dia.

O sweep de pool fica para a etapa 5, isolado, onde é a conclusão em vez do vício.

---

### 8. `201` significa **durável** nas três engines

O shard não responde em memória: agrupa até 256 lances (ou 2ms) num commit e só então libera as respostas.

**Por quê:** esta é a decisão mais estrutural do documento. Se o shard confirmasse antes de persistir, parte da vitória dele viria de ter feito uma **promessa mais fraca**, não de eliminar contenção — e comparar `p95 12ms` com `p95 890ms` seria comparar coisas diferentes. É a primeira coisa que um revisor sério apontaria. Além disso, o invariante *"todo 201 está no banco"* seria literalmente falso com o journal cheio no momento do crash.

**O que se ganha em troca:** um único escritor pode amortizar o `fsync` sobre centenas de lances, o que é uma afirmação de engenharia mais forte que "respondi antes de gravar". O gap entre `bid_accept_duration_seconds` e `bid_confirm_duration_seconds` expõe o custo da durabilidade como métrica, em vez de escondê-lo no contrato.

---

### 9. `409` e `422` têm o mesmo envelope e são ambos retentáveis

**Por quê:** pessimista e shard nunca produzem `409` — elas serializam, e o perdedor recebe `422`. Se `422` fosse terminal, um VU no modo shard mandaria uma requisição e desistiria enquanto o mesmo VU no modo otimista mandaria dez retentativas. As três receberiam cargas diferentes e `aceitos/s` deixaria de comparar qualquer coisa.

`409` e `422` são o mesmo evento visto por mecanismos diferentes: *alguém me passou na frente*. A distinção existe para a métrica, não para a decisão do cliente.

---

### 10. `Outcome` no `BidResult`; `error` só para falha de infraestrutura

**Por quê:** rejeição de lance não é erro — "você perdeu" é uma resposta bem-sucedida da engine. Com erros tipados, o handler precisaria de `errors.As` no caminho mais quente do sistema e ganharia um `case` novo a cada engine. Com `Outcome`, o handler é uma tabela de mapeamento e trocar `BID_STRATEGY` não toca uma linha de HTTP — que é condição do experimento, não estética.

`Current` é preenchido **sempre**, inclusive em rejeição: é dele que sai o envelope da decisão 9.

---

### 11. Suíte de conformidade escrita contra a interface, com `testcontainers`

`enginetest.RunConformance(t, engine)`, Postgres real, um contêiner por pacote.

**Por quê:** escrita agora, faz as etapas 2 e 3 custarem duas linhas de teste cada — a engine nova passa na suíte ou está errada. Escrita depois, cada engine ganha teste sob medida e o projeto perde a garantia de que as três respeitam o mesmo contrato, que é a premissa de compará-las.

Escrita contra `BidEngine` e **nunca contra SQL**, senão a engine em memória do shard não poderia ser validada pela mesma suíte.

É diferente do `cmd/checker`: a suíte roda em segundos no CI e prova a engine; o checker roda pós-k6 e prova o sistema.

---

### 12. Fechamento é propriedade do tempo, não evento

`now() < ends_at` guarda a query. `status` entra no schema agora mas só é escrito na etapa 4.

**Por quê:** se o fechamento dependesse de um worker que só existe na etapa 4, o invariante *"nenhum lance depois do fechamento"* ficaria três etapas sem cobertura, e o `WHERE` carregaria uma guarda morta. Com `ends_at`, o invariante vale desde a etapa 1 sem worker nenhum.

**Consequência de qualidade:** o `closerd` deixa de ser responsável pela corretude e passa a ser responsável só por performance. Matá-lo no teste de caos não pode deixar entrar lance atrasado — no máximo atrasa a materialização.

---

### 13. Cada célula da matriz parte do mesmo estado

`TRUNCATE` + `cmd/seed` + `VACUUM ANALYZE` + warmup descartado. A primeira célula é repetida por último como célula-controle.

**Por quê:** 36 células na mesma instância. Sem reset, a última escreveria numa tabela com milhões de linhas e estatísticas velhas enquanto a primeira escreveu numa tabela vazia — a última estratégia da lista pareceria a pior por efeito de ordem. Isso invalidaria o gráfico principal do projeto. A célula-controle é a prova de que não aconteceu.

---

### 14. Identidade por `X-User-Id`, sem JWT

**Por quê:** autenticação é custo constante e idêntico nas três estratégias, ou seja, não move nenhuma curva. Pela regra de escopo, fica de fora. Quando entrar, entra como middleware e nada abaixo dele muda.

---

### 15. `idempotency_key` já na migration `001`

Coluna e índice único parcial nascem na etapa 1, embora a idempotência só chegue na etapa 2.

**Por quê:** criar índice único numa tabela com milhões de linhas depois é caro e ruidoso; carregar uma coluna `NULL` até lá custa zero. O índice parcial mantém fora dele todas as linhas da etapa 1.

Redis é a primeira linha de defesa contra duplicata; a restrição no banco é a rede embaixo, para quando o Redis reiniciar ou uma chave expirar cedo.

---

### 16. Métricas entram junto com a engine que as alimenta

Etapa 1 leva `bid_confirm_duration_seconds`, `bid_outcomes_total{outcome}` e as do pool do pgx.

**Por quê:** declarar as doze na etapa 1 produziria um dashboard com painéis vazios e nenhuma forma de saber se estão vazios por design ou por bug. As do pool são obrigatórias já agora: sem elas, o custo do pessimista é indistinguível de pool subdimensionado.

`bid_conflicts_total` não existe como métrica própria — é um label de `bid_outcomes_total`. E `bid_attempts_per_accept` mora no k6 na etapa 1, porque o servidor não tem como correlacionar três requisições como tentativas do mesmo lance antes de existir chave de idempotência; migra para o servidor na etapa 2.

---

### 17. `min_increment_cents` é do leilão, não do cliente

`minNextBid = highest_bid_cents + min_increment_cents`, calculado pelo servidor.

**Por quê:** se cada cliente escolhesse o próprio incremento, dois VUs competiriam sob regras diferentes e a comparação entre estratégias herdaria essa assimetria.

---

### 18. `MAX_RETRIES = 10`, `BID_DEADLINE = 2s`, o que vier primeiro

E `bids_exhausted` é número de manchete do relatório.

**Por quê:** um apostador real desiste por tempo, não por contagem; os dois limites juntos modelam isso. `bids_exhausted` **é** o colapso do otimista tornado visível — se a taxa passar de ~20%, `MAX_RETRIES` está mascarando o efeito e precisa subir.
