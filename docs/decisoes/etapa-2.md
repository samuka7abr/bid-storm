# Decisões — Etapa 2

[← índice](../README.md)

Registro das decisões tomadas ao desenhar as specs da etapa 2. A numeração continua a da [etapa 1](etapa-1.md): as decisões de 1 a 24 continuam valendo, e as que são emendadas aqui dizem em qual ponto.

---

## Decisões da spec 01

Tomadas ao desenhar a [spec 01 da etapa 2](../specs/etapa-2/01-spec-engine-pessimista.md), a engine pessimista.

---

### 25. A pessimista paga a transação inteira, e nada além dela

`Begin` → `SELECT ... FOR UPDATE` → CTE de escrita → `Commit`. Quatro round-trips no aceite, três na rejeição, conexão presa do primeiro ao último.

**Por quê:** existe uma forma de escrever a pessimista em um statement só, com `FOR UPDATE` dentro de uma CTE, e ela funciona. Mas o que ela implementa não é mais pessimismo: o lock nasce e morre dentro do statement, a decisão volta para dentro do SQL, e o que sobra é a engine otimista com outro mecanismo de serialização. O projeto não pergunta *qual SQL é mais rápido*, pergunta **o que acontece quando um cliente segura um lock enquanto decide** — e é essa a forma que a etapa 5 vai medir.

A decisão 19 já fixou o outro lado: o otimista não ganha uma transação de graça, porque precisar de transação é um custo real do pessimismo. Esta decisão é a mesma regra vista do outro lado — o pessimismo paga o custo que é dele, e não paga nenhum que não seja.

**O que ficou de fora por não ser do mecanismo:** `UPDATE` e `INSERT` vão numa CTE só, como no otimista, em vez de dois round-trips separados. E o `UPDATE` não repete as guardas de `status` e `ends_at`: o lock já garante que o estado lido é o vigente, e repetir a guarda criaria um ramo "impossível" que ninguém saberia classificar se um dia fosse alcançado.

**Consequência que precisa ser encarada:** se a pessimista perder no gráfico, a primeira pergunta hostil será *"ela perdeu por mecanismo ou pelos quatro round-trips?"*. A resposta é a decisão 26, e ela é medida, não argumentada.

---

### 26. `lock_wait_duration_seconds` compartilha os buckets de `bid_confirm_duration_seconds`

Mesmos catorze buckets exponenciais, de 1ms a 8,192s.

**Por quê:** as duas séries só respondem à pergunta que interessa se puderem ser lidas uma contra a outra, bucket a bucket. Se sob alta contenção a espera em lock domina a confirmação, os quatro round-trips da decisão 25 são ruído dentro do número, e a crítica morre no gráfico — que é a mesma manobra da decisão 6 com `RETRY_POLICY`. Se **não** dominar, o projeto descobre isso antes de publicar, e não depois.

Buckets diferentes obrigariam a comparação a passar por interpolação de quantis, que é precisamente onde uma diferença de dez pontos percentuais se esconde.

**Limite registrado:** o histograma mede o `SELECT ... FOR UPDATE` inteiro — round-trip, planejamento e espera — e não a espera pura. Extrair a espera pura exigiria amostrar `pg_locks`, ou seja, custo no caminho quente para medir o caminho quente. Não é preciso: o piso do histograma na célula de 1000 leilões, onde ninguém disputa a mesma linha, **é** o custo do round-trip. A calibração vem da própria matriz.

---

### 27. Dentro de transação, `now()` é o instante do `BEGIN` — a pessimista lê `clock_timestamp()`

Emenda a decisão 22 para o caso transacional: a autoridade continua sendo o relógio do Postgres, mas a função muda.

**Por quê:** `now()` é `transaction_timestamp()`. No otimista isso é inofensivo, porque cada statement é a própria transação. Na pessimista, o `BEGIN` acontece **antes** da espera em lock: sob mil VUs, a transação pode ficar centenas de milissegundos na fila e ainda assim ler um `now()` anterior à espera. A guarda de fechamento ficaria mais frouxa que a do otimista, e a diferença apareceria exatamente na borda do `ends_at` — o cenário inteiro do projeto — parecendo diferença entre estratégias quando é diferença entre relógios.

`clock_timestamp()` é avaliado durante a execução do statement, depois da espera. A pessimista fica com a guarda pelo menos tão estrita quanto a do otimista.

**Efeito colateral bom:** `bids.created_at` continua no `DEFAULT now()`, ou seja, o instante do `BEGIN`, que é anterior ao `clock_timestamp()` que autorizou o aceite. Logo `created_at < ends_at` vale por construção sempre que o lance passa, e o invariante I4 do `cmd/checker` — *nenhum lance depois do fechamento* — não pode acusar falso positivo por causa do lock.

---

### 28. A pessimista instrumenta o próprio lock, e é a segunda exceção registrada da decisão 23

`lock_wait_duration_seconds` é observada de dentro da engine. `bid_confirm_duration_seconds` e `bid_outcomes_total` continuam vindo do decorator, nas três.

**Por quê:** o decorator mede a fronteira do `BidEngine`, e a espera em lock é um intervalo interno que ele não tem como enxergar. É a mesma situação já prevista para `bid_accept_duration_seconds` no shard da etapa 3, e a regra que a decisão 23 protege continua intacta: o que **compara** as três estratégias é medido num lugar só; o que descreve o mecanismo de uma delas é medido dentro dela.

O teste do limite é simples: nenhuma série instrumentada por dentro pode aparecer no eixo de comparação da etapa 5. `lock_wait` explica a pessimista; não julga o otimista.

**Sem label `strategy`:** só uma engine produz a série. Um label com um valor único sugeriria que as outras duas reportam zero, quando na verdade elas não reportam nada — e zero é uma afirmação diferente de silêncio.

---

### 29. Nenhuma retentativa por deadlock, porque a pessimista não pode entrar em deadlock

Sem laço de retry, sem tratamento de `40P01`.

**Por quê:** cada transação tranca **uma** linha de `auctions` e nunca uma segunda. O `INSERT` em `bids` toma `FOR KEY SHARE` sobre a mesma linha que a transação já detém em modo mais forte, na mesma transação. Sem duas linhas, não existe ordem de aquisição para inverter, e sem inversão não existe ciclo.

Um laço de retry aqui seria código que nunca executa — e pior: se um dia a engine passar a trancar duas linhas, o deadlock precisa aparecer **alto**, no log e na métrica, em vez de ser absorvido em silêncio por uma retentativa que faria o benchmark medir uma engine diferente da descrita.

**Corolário verificável:** sob a pessimista, `UNIQUE (auction_id, seq)` é matematicamente inalcançável, porque só o dono do lock calcula `seq`. Se essa restrição disparar alguma vez, não é contenção: é bug de engine, e a restrição permanece no schema como asserção viva.

---

### 30. `READ COMMITTED`, não `SERIALIZABLE`

A transação da pessimista roda no isolamento padrão.

**Por quê:** `SERIALIZABLE` transformaria toda disputa em `40001` mais retentativa. Isso não é a estratégia pessimista com mais segurança — é a **estratégia otimista com a retentativa movida para dentro do banco**, com amplificação de retry, colapso sob contenção e tudo o mais que a hipótese atribui ao otimista. Seria uma quarta célula no eixo de estratégias, respondendo a uma pergunta que este projeto não fez, e cujo lugar natural é o trabalho seguinte.

Em `READ COMMITTED`, `SELECT ... FOR UPDATE` já entrega o que a estratégia promete: quem detém o lock lê a última versão comitada e é o único que escreve até o `COMMIT`.

**Fica registrado como não medido:** o projeto não afirma nada sobre `SERIALIZABLE`. Afirmar sem medir seria exatamente o que os quatro invariantes de método existem para impedir.
