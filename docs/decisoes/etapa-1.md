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

---

## Emendas da spec 02

As decisões de 1 a 18 foram tomadas antes da primeira linha de código. As de 19 a 24 foram tomadas ao desenhar a [spec 02](../specs/etapa-1/02-spec-engine-otimista.md), e quatro delas **emendam** o que já estava publicado em `projeto/estrategias.md` e `projeto/api.md`. Onde houver divergência, vale o que está aqui.

---

### 19. O caminho feliz do otimista é um statement, não uma transação

`UPDATE` e `INSERT` numa CTE só, sem `Begin`/`Commit`. Emenda o código ilustrativo de [estrategias.md](../projeto/estrategias.md#otimista).

**Por quê:** a versão com transação são quatro round-trips por tentativa, com a conexão presa nos quatro. A CTE é um, e continua atômica.

A pessimista não pode fazer o mesmo, porque `SELECT ... FOR UPDATE` exige transação aberta. Isso é deliberado: **precisar de transação é um custo real do pessimismo**, e dar uma transação de graça ao otimista para nivelar os dois esconderia esse custo e pesaria a balança a favor da hipótese do projeto. Os quatro invariantes de método fixam pool, CPU, memória e estado inicial — nenhum deles pede paridade de round-trips.

**Consequência:** o otimista entra no benchmark na melhor forma que o mecanismo permite. Se ele colapsar mesmo assim, o resultado é adverso à hipótese e sobrevive ao revisor mais hostil, que é a única espécie de resultado que vale publicar.

---

### 20. `Conflict` tem precedência sobre `TooLow`

A ordem do `classify` passa a ser `NotFound` → `Closed` → `Conflict` → `TooLow`. Emenda a ordem de [estrategias.md](../projeto/estrategias.md#otimista), que checava o valor antes da versão.

**Por quê:** a ordem original esvazia a métrica-chave do otimista. O apostador da decisão 5 re-mira em `minNextBid`; enquanto a requisição viaja, outro VU passa na frente; a rejeição chega com versão velha **e** valor abaixo do novo mínimo. Sob alta contenção esse é o caso quase universal, e com o valor sendo checado primeiro toda rejeição vira `too_low` — `bid_outcomes_total{outcome="conflict"}` vai a zero exatamente na célula em que a tese precisa dela, e o otimista passa a produzir a mesma tabela de desfechos que o pessimista.

Com a ordem invertida, `Conflict` significa *o snapshot do cliente ficou velho* e `TooLow` significa *o cliente estava atualizado e ainda assim mandou pouco*.

**Não reabre a decisão 1.** O lance de R$ 5 num leilão de R$ 9.000 não vira retentativa infinita, porque o corpo carrega `minNextBid` e o apostador re-mira nele, e porque `MAX_RETRIES` e `BID_DEADLINE` limitam de qualquer forma. A decisão 1 proíbe colapsar os quatro desfechos num só; a ordem entre eles é outra escolha.

---

### 21. `Current` só existe quando o leilão existe

`Outcome` ganha `Invalid` (400). `NotFound`, `Invalid` e erro de infra respondem com um `ErrorResponse` sem estado. Emenda `Current AuctionState // SEMPRE preenchido` de [estrategias.md](../projeto/estrategias.md#a-interface) e a última linha da decisão 10, e acrescenta uma linha à tabela de [api.md](../projeto/api.md#rejeitado).

**Por quê:** "sempre preenchido" é literalmente impossível em `NotFound` — não há leilão de onde tirar estado. Publicar `currentHighestBid: 0` num `404` seria o contrato afirmando que um leilão inexistente vale zero centavos, e o painel da etapa 5b renderizaria esse zero.

`Invalid` existe porque `expectedVersion` é obrigatório só no otimista, e `api.md` proíbe o handler de ramificar por estratégia — condição do experimento, já que um `switch` no handler faria a comparação medir também o handler. Com o desfecho vindo da engine, o handler continua sendo uma tabela de mapeamento e a interface continua com um método só.

**Consequência de qualidade:** `AuctionStateView` é embutida nas structs de `201`, `409`, `422` e `410`, então o compilador garante o que a decisão 2 exige — nenhuma dessas quatro respostas pode nascer sem estado. E `bid_outcomes_total{outcome="invalid"}` acima de zero durante o benchmark denuncia k6 mal configurado, de graça.

---

### 22. O relógio do Postgres é a autoridade sobre `ends_at`

O `SELECT` do `classify` e o de `GET /auctions/:id` devolvem `now()` junto das colunas, e é esse instante que alimenta `IsClosed`. Emenda o `time.Now()` do `classify` em [estrategias.md](../projeto/estrategias.md#otimista).

**Por quê:** o `UPDATE` decide o aceite com `now() < ends_at`, ou seja, com o relógio do Postgres. Se a classificação decidisse com o relógio do container do `auctiond`, um skew de poucos milissegundos produziria `410 auction_closed` com `retryable: false` para um lance que o banco teria aceito, e o VU desistiria de um lance válido.

Isso morde na borda do `ends_at`, que é o cenário inteiro do projeto — mil pessoas no último segundo. O erro seria enviesado, não aleatório, e apareceria como uma diferença entre estratégias que na verdade é uma diferença entre relógios.

**Custo:** zero. A coluna extra vem no `SELECT` que já roda.

---

### 23. As métricas de lance são observadas por um decorator sobre `BidEngine`

`Instrument(next BidEngine, reg, strategy)` embrulha qualquer implementação. Nenhuma engine chama Prometheus por dentro.

**Por quê:** se cada engine instrumentasse a si mesma, nada no compilador impediria o shard de cronometrar a partir de um ponto mais generoso que o otimista. Esse é o viés silencioso que os quatro invariantes de método existem para impedir, e é a primeira coisa que um revisor procuraria ao ver o shard ganhar. Com o decorator, as três são medidas na mesma fronteira porque só existe uma fronteira.

**Exceção registrada:** `bid_accept_duration_seconds` — o instante da decisão em memória — só existe dentro do shard, e é instrumentada por dentro dele na etapa 3. `confirm` continua vindo do decorator nas três, que é o que torna o gap da decisão 8 uma comparação honesta.

---

### 24. A suíte de conformidade valida a história inteira por replay

Depois do teste de concorrência, a suíte lê os lances em ordem de `seq` e reexecuta a regra a cada passo: sequência `1..A` sem buraco, cada `amount_cents` maior ou igual ao anterior mais `min_increment_cents`, total batendo com os `Accepted` contados, estado reconstruído batendo com a linha de `auctions`.

**Por quê:** asserir apenas que `seq` é único e crescente prova o `UNIQUE (auction_id, seq)`, não a engine — a suíte estaria confirmando o Postgres.

O alvo real é o shard da etapa 3. O otimista carrega `highest_bid_cents + min_increment_cents <= $1` dentro do `WHERE`, então o banco o impede de aceitar lance inválido. O shard decide em memória, sem `WHERE` nenhum: uma corrida ali aceitaria um lance abaixo do mínimo no meio da história, deixaria a sequência perfeita, e passaria despercebida por qualquer asserção mais fraca — para reaparecer como um número estranho no `cmd/checker`, depois do benchmark.

É o que faz a decisão 11 valer o que promete: a engine nova passa na suíte ou está errada.
