# Etapa 2 — Spec 02: Idempotência

[← índice](../../README.md) · [decisões da etapa 1](../../decisoes/etapa-1.md) · [decisões da etapa 2](../../decisoes/etapa-2.md) · [spec 01](01-spec-engine-pessimista.md) · [provas](../../projeto/provas.md)

## Contexto

A spec 01 entregou a segunda curva e provou o que a decisão 11 tinha comprado: uma engine nova cabe em 7 arquivos porque o contrato, o envelope, o decorator e a suíte já existiam. Esta é a spec do meio da etapa 2, e é a única das três que mexe no **caminho quente das três engines ao mesmo tempo**.

É por isso que ela nasce agora, entre a pessimista e o shard, e não depois das três: aqui o middleware é validado contra duas engines de uma vez, e a terceira nasce já dentro dele. Depois da etapa 3, a mesma mudança chegaria como alteração no caminho quente de três engines já medidas.

Duas promessas do projeto vencem nesta spec, e elas foram escritas em documentos diferentes sem que ninguém tivesse checado se cabem juntas:

- [provas.md §3](../../projeto/provas.md): o gerador reenvia 10% das requisições com a mesma `X-Idempotency-Key`, e o relatório mostra **zero lances duplos**. *"A idempotência não é descrita, é demonstrada."*
- [observabilidade.md](../../projeto/observabilidade.md), linhas 15 e 31: `bid_attempts_per_accept` migra do k6 para o servidor na etapa 2, porque *"a chave de idempotência dá esse fio"* que correlaciona três requisições como tentativas do mesmo lance.

A primeira quer que a chave nomeie **uma requisição** — é assim que toda API de pagamento a define, e é o que faz um reenvio idêntico devolver a resposta guardada. A segunda quer que a chave nomeie **um lance lógico**, atravessando retentativas que, pelo apostador da decisão 5, carregam corpos diferentes a cada vez.

As duas leituras não convivem. Escolher entre elas, com o preço da escolha dito em voz alta, é o trabalho central desta spec (decisão 31), e tudo o mais aqui é consequência dela.

Sustentam as decisões abaixo a **2** (toda resposta carrega estado), a **5** (o apostador re-mira), a **9** (`409` e `422` são o mesmo evento), a **13** (cada célula parte do mesmo estado), a **15** (o índice único parcial é a rede embaixo do Redis), a **16** (métrica entra junto de quem a alimenta), a **18** (`MAX_RETRIES` e `BID_DEADLINE`), a **21** (nenhum handler ramifica por estratégia) e a **23** (uma fronteira só para o que compara as três), mais as decisões **31** a **41**, tomadas nesta spec.

## Objetivo

Pôr a idempotência no caminho de todo lance, de forma idêntica nas três engines, e transformar a chave no fio que faz a amplificação de retentativa ser medida pelo servidor.

O sistema deve:

- Barrar duplicata sem que nenhum handler e nenhuma engine saibam que a idempotência existe: um middleware acima do switch de estratégia
- Devolver, para o reenvio de um lance já aceito, o **mesmo `201` byte a byte**, marcado como replay
- Responder a duplicata em voo **na hora**, com um código que não seja `409` e sem esperar por nada
- Publicar `idempotency_hits_total{strategy,kind}` e `bid_attempts_per_accept{strategy}`, esta última alimentada pela contagem de tentativas sob uma chave
- Gravar a chave em `bids.idempotency_key`, ligando pela primeira vez o índice único parcial que a etapa 1 criou vazio
- Falhar fechado quando o Redis não responder, e dizer isso no `/readyz`
- Continuar valendo sem chave nenhuma: sem o header, o middleware é passagem e o Redis não é tocado

## Fora de Escopo

- **Duplicatas injetadas no k6**, o contador de replays do lado do cliente e a retentativa sobre erro de transporte — spec 03. Aqui o k6 passa a **mandar** a chave; ele ainda não a reenvia de propósito
- **O invariante de chave repetida (I7) no `cmd/checker`** e o **aperto do I5** — spec 03, pela decisão 40. Esta spec entrega a condição, não o aperto
- `cmd/checker` e `cmd/seed` não mudam, em linha nenhuma
- `migrations/` não muda: a coluna e o índice existem desde a `001` (decisão 15), e esta spec é quem finalmente escreve neles
- Engine shard — etapa 3. `BID_STRATEGY=shard` continua falhando no boot nomeando a etapa
- Redis Streams, `closerd`, `stream_pending_entries` — etapa 4. Esta spec traz o Redis ao processo, e usa dele apenas `EVAL`, `HSET`, `HDEL`, `PEXPIRE` e `PING`
- Cache de resposta para `GET /auctions/:id` — o middleware é montado na rota de lance e em nenhuma outra
- Dashboards do Grafana — etapa 5
- Qualquer variável de ambiente nova: `REDIS_URL` já existe em `internal/config` desde a etapa 1, com default

## Fluxo

```text
POST /auctions/:id/bids
  └── RequireUserID                      ← inalterado: 400 se X-User-Id não é uuid
  └── idem.Middleware                    ← acima do switch: não sabe qual engine responde
        ├── sem X-Idempotency-Key        → passa direto, e nada toca o Redis
        ├── chave malformada             → 400 invalid_idempotency_key
        │
        ├── claim: EVAL do script (1 round-trip, atômico)
        │     ├── erro de Redis          → 503 unavailable, retryable      (falha fechado)
        │     ├── campo done presente    → 201 guardado, verbatim          hits{kind="replayed"}
        │     │                             + X-Idempotency-Replayed: true
        │     ├── campo busy presente    → 425 idempotency_in_flight       hits{kind="in_flight"}
        │     └── nenhum dos dois        → busy=1, attempts=n, PEXPIRE 30s
        │
        ├── PlaceBid → a engine da etapa 1 ou da spec 01, sem uma linha de diferença
        │     └── instrumented: bid_confirm_duration_seconds, bid_outcomes_total
        │
        └── finish, em defer (1 round-trip)
              ├── 201  → HSET done=<corpo> · HDEL busy · PEXPIRE 5min
              │           bid_attempts_per_accept{strategy}.Observe(n)
              └── resto → HDEL busy
                          (a entrada sobrevive com o attempts, e é isso
                           que faz a retentativa re-mirada contar como n+1)
```

O middleware nunca chama a engine duas vezes e nunca chama o Redis mais de duas: **um round-trip na entrada, um na saída**, iguais nas três estratégias.

## Decisoes Tecnicas

### A chave nomeia o lance lógico, não a requisição

É a decisão da qual o resto pende, e ela contraria o que a palavra "idempotência" faz esperar.

Numa API de pagamento a chave nomeia **uma requisição**: mesma chave e mesmo corpo devolvem a resposta guardada, mesma chave e corpo diferente devolvem erro — o cliente reusou a chave para outra coisa, e responder qualquer outra coisa seria cobrar duas vezes ou mentir. É desenho correto, e aqui ele é **impossível**.

O apostador da decisão 5 re-mira a cada rejeição: `amount = minNextBid` da resposta que acabou de chegar. A tentativa 2 do mesmo lance lógico tem, **por construção**, um corpo diferente da tentativa 1. Sob a semântica de requisição, ou o cliente troca de chave a cada tentativa — e aí não existe fio nenhum correlacionando as três, que é exatamente o que observabilidade.md:31 diz faltar — ou ele mantém a chave e a tentativa 2 volta como erro de reuso, deixando o apostador parado. As duas saídas matam uma promessa do projeto.

Então a chave nomeia o **lance lógico**: um VU, uma iteração, uma chave, mantida por todas as tentativas até o `201` ou até a desistência. Sem impressão digital do corpo, porque o corpo *deve* mudar.

**O preço, dito por inteiro:** um cliente que reusar uma chave para um lance genuinamente diferente recebe de volta a resposta do primeiro. Não há como este desenho distinguir os dois casos, e fingir que há seria pior. O que sobra no lugar da impressão digital é o formato da chave — UUID, validado como `X-User-Id` (decisão 32) — que torna a colisão acidental um evento que não acontece.

**E a consequência que faz o desenho funcionar:** se a chave não distingue duplicata de retentativa pelo corpo, o que as distingue? O **tempo**. Uma retentativa honesta só parte depois que a resposta anterior chegou; uma duplicata é, por definição, concorrente com a original ou posterior a um lance já encerrado. Daí:

| O que o Redis encontra | O que isso é | Resposta |
| --- | --- | --- |
| `done` gravado | reenvio de um lance já aceito | o `201` guardado, verbatim |
| `busy` marcado | outra requisição com esta chave está executando **agora** | `425` |
| nem um nem outro | primeira tentativa, ou retentativa re-mirada | segue para a engine, como tentativa `n` |

A marca de "em voo" não é otimização de corrida: ela é a **definição de duplicata** nesta API.

### Duplicata em voo responde `425`, na hora, e nunca `409`

provas.md registrou a opção como *"o segundo request aguarda ou recebe `409 Idempotency-In-Flight`"*. As duas metades ficam para trás, e por motivos diferentes.

**Não pode ser `409`.** Esse código já significa `version_conflict` neste contrato, o apostador do k6 conta todo `409` em `bids_conflict` e absorve `currentVersion`/`minNextBid` do corpo. Um `409` de idempotência não tem estado de leilão para carregar — quebraria a decisão 2 — e envenenaria a série mais central da tese com eventos que não são disputa nenhuma. Um contador que mistura "alguém passou na sua frente" com "você mandou a mesma coisa duas vezes" não mede amplificação de retentativa: mede o gerador.

**Não pode esperar.** Esperar significa segurar a requisição até a original terminar, e a original demora o que a **engine** demora. Sob a pessimista com o lock disputado, a original espera 300ms; a duplicata esperaria os mesmos 300ms mais o custo de descobrir que acabou. O custo da idempotência deixaria de ser constante e passaria a escalar com a latência da engine — ou seja, o middleware entraria no eixo de comparação das três curvas pela porta dos fundos. Um curto-circuito imediato custa os mesmos dois round-trips de Redis em qualquer engine.

Fica `425 Too Early`, com `error: "idempotency_in_flight"`, `retryable: true` e o envelope sem estado (`ErrorResponse`, o mesmo do `404` e do `503`): não há estado a publicar, porque nenhuma engine foi consultada. É código que o k6 ainda não espera — em `expectedStatuses` — e não precisa esperar nesta spec, porque nesta spec ele não manda duplicata. Quem o ensina é a spec 03, junto da injeção.

### Só o `201` é guardado, porque só ele escreve linha

O `done` da entrada só é preenchido quando o desfecho é `Accepted`. Rejeição não guarda nada.

Não é economia: é que **rejeição é idempotente por natureza**. Um `422` reenviado continua `422`, porque `minNextBid` só cresce; um `409` reenviado continua `409`, com a versão ainda mais velha; `410` e `404` são propriedades do leilão, não da requisição. Replay de rejeição custaria uma entrada no Redis para devolver o que a engine devolveria de qualquer forma, e o que a idempotência existe para impedir — a **segunda linha em `bids`** — só pode nascer de um aceite.

O que a saída faz na rejeição é uma coisa só, e é obrigatória: apagar `busy`. Sem isso, a retentativa re-mirada que chega 40ms depois encontraria a chave ocupada e levaria `425` — o apostador ficaria preso contra o próprio middleware. O `attempts` fica, e é dele que sai o `n+1` da tentativa seguinte.

### O replay é o mesmo `201`, byte a byte, e se declara num header

O middleware embrulha o `gin.ResponseWriter` e guarda os bytes que o handler escreveu. O reenvio recebe **exatamente** aquele corpo: mesmo `bidId`, mesmo `seq`, mesmo `currentVersion`. Um replay que recalculasse o corpo a partir do estado atual devolveria um `minNextBid` de agora dentro da resposta de um lance de antes, e o cliente não teria como saber qual dos dois números leu.

Reconstruir o envelope em vez de guardá-lo — pedindo à engine, ou lendo `bids` pela chave — foi descartado justamente por isso, e por um segundo motivo: ambos custam round-trip ao Postgres no caminho que existe para **não** ter custo.

A marca vai num header, `X-Idempotency-Replayed: true`, e não num campo do corpo: o corpo tem que permanecer idêntico, e um campo a mais nele seria a diferença que a decisão acabou de proibir. Esse header é a condição que a spec 03 vai consumir para apertar o I5 (decisão 40) — sem ele, o cliente não tem como contar um replay separado de um aceite.

### A entrada é um hash, e o claim é um script

A entrada é um hash em `idem:<uuid>` com três campos: `attempts`, `busy` e `done`.

```lua
-- KEYS[1] = idem:<uuid>   ARGV[1] = TTL em voo, em ms
local done = redis.call('HGET', KEYS[1], 'done')
if done then return {'replay', done} end
if redis.call('HGET', KEYS[1], 'busy') then return {'busy'} end
local n = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
redis.call('HSET', KEYS[1], 'busy', '1')
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return {'go', n}
```

Um `EVALSHA`, um round-trip, atômico por construção — o Redis roda o script inteiro sem intercalar comando de ninguém.

**A alternativa descartada** era compor comandos avulsos: `SET NX` para tomar a marca, `GET` quando o `NX` falha, `INCR` para a contagem. São dois a três round-trips, e o `INCR` fica do lado errado da decisão: ele contaria também as duplicatas barradas, que nunca chegaram à engine e portanto não são tentativas de nada. Contar tentativa no mesmo passo atômico em que se concede a passagem é o que faz `bid_attempts_per_accept` significar *"requisições que chegaram à engine sob esta chave até uma ser aceita"* e não *"requisições que carregaram esta chave"*.

**Os dois TTLs** existem por razões independentes:

| TTL | Valor | Por quê |
| --- | --- | --- |
| em voo | 30s | é o teto do estrago de um processo que morra entre o claim e o finish: a chave fica presa por 30s, não para sempre. `BID_DEADLINE` é 2s, então nenhuma requisição viva é atingida. É o número que provas.md já tinha fixado |
| terminal | 5min | maior que o cenário mais longo do projeto (`ramp`, 2min), então dentro de uma célula nenhuma entrada terminal expira e nenhuma duplicata escapa por vencimento |

O finish é um pipeline: `HSET done` + `HDEL busy` + `PEXPIRE` no aceite, `HDEL busy` na rejeição. Um round-trip, e ele vai num `defer` — se o handler entrar em pânico, o `gin.Recovery()` que está por fora só recupera depois que este `defer` correu, e a chave não fica ocupada por 30s por causa de um bug.

### Redis fora do ar falha fechado, e o `/readyz` diz

Erro de Redis no claim vira `503 unavailable`, `retryable: true`, com uma linha de log — a mesma exceção do `503` do handler, pelo mesmo motivo: é raro e não se explica sozinho.

Falhar aberto (deixar o lance passar sem idempotência) seria o sistema fazendo uma **promessa mais fraca em silêncio**, que é a coisa que a decisão 8 existe para proibir. E o efeito colateral seria pior que o `503`: sem a marca em voo, as duplicatas concorrentes chegariam às duas engines, ambas escreveriam, e o índice único parcial recusaria a segunda com violação de unicidade — um `503` de qualquer forma, só que depois de trabalho gasto e com a transação abortada.

`/readyz` ganha a terceira condição, `redis`, ao lado de `database` e `schema`, com o mesmo formato de resposta que nomeia qual falhou. `/healthz` continua respondendo 200 sem tocar em nada: infraestrutura divergente não é processo morto, e reiniciar não conserta (etapa 1). Isso também prepara o cenário de caos da etapa 4, em que o Redis é pausado por 5 segundos e o compromisso registrado é *"requisições falham de forma limpa"*.

**O que conta como hit:** só duplicata efetivamente barrada, `replayed` ou `in_flight`. Redis caído produz **zero** hits, e isso é a verdade — nada foi barrado. O critério é o mesmo que a spec 01 aplicou ao histograma de lock: falha de infraestrutura não vira amostra.

### As engines gravam a chave, e a rede embaixo passa a existir

`bids.idempotency_key` e `bids_idempotency_key_uq` estão no schema desde a `001` e nunca receberam um valor. A decisão 15 chamou o índice de *"a rede embaixo, para quando o Redis reiniciar ou uma chave expirar cedo"* — uma rede que ninguém tece não é rede.

As duas engines passam a escrever a chave no `INSERT`, com `NULLIF($n, '')`: lance sem chave grava `NULL` e continua fora do índice parcial, exatamente como a decisão 15 desenhou. São duas linhas de SQL em cada `sql.go` e um parâmetro a mais.

A suíte de conformidade ganha o caso correspondente — *um aceite grava a chave que veio na requisição; um aceite sem chave grava `NULL`* —, e é isso que torna a obrigação executável para o shard da etapa 3, que decide em memória e não tem `WHERE` nenhum para lembrá-lo. A fábrica da suíte (`func(*pgxpool.Pool) bid.BidEngine`) **não muda**: o caso novo é um `t.Run` que usa a engine que a fábrica já devolveu.

Isso não asserta SQL contra uma engine específica. É a mesma leitura da história de volta que a decisão 24 já faz, aplicada a mais uma coluna.

### Observabilidade

Entram as duas séries que a decisão 16 alocou à etapa 2 junto do mecanismo que as alimenta:

| Série | Tipo | Labels | Buckets |
| --- | --- | --- | --- |
| `idempotency_hits_total` | Counter | `strategy`, `kind` | — |
| `bid_attempts_per_accept` | Histogram | `strategy` | `1..10`, lineares |

**`kind` em vez de duas séries.** `replayed` e `in_flight` são duplicatas barradas por caminhos diferentes, e alguém que quisesse o total teria que somar duas séries e esquecer uma. É a mesma manobra do `bid_conflicts_total`, que não existe porque é `bid_outcomes_total{outcome="conflict"}`. Os dois filhos são ligados no boot, como o `confirm` da etapa 1: o processo roda uma estratégia só, e `/metrics` nunca deve parecer que a série foi esquecida quando o que houve foi ausência de duplicata.

**`strategy` nas duas, e por que isso não fere a decisão 23.** O middleware está **acima** do switch de estratégia: ele é um componente só, idêntico para as três, que não sabe e não pode saber qual engine está atrás. Não é cada engine se medindo — é uma segunda fronteira única, e a regra que a decisão 23 protege continua de pé. O label existe pela mesma razão mecânica de `bid_outcomes_total{strategy}`: sem ele, o dashboard da etapa 5 não consegue sobrepor três curvas vindas de 36 execuções.

**Buckets de 1 a 10.** `MAX_RETRIES` é 10 (decisão 18), então nada pode cair acima do último bucket. O `+Inf` fica sendo, de graça, o detector de um cliente que violou o próprio limite.

**A amostra sai no aceite, e só nele.** Lance que se esgota não produz amostra — é `bids_exhausted` que o conta, e misturar os dois daria um número que não é nem amplificação nem taxa de desistência. É a mesma regra que o k6 já aplica.

### A série do k6 não some: as duas medições convivem

observabilidade.md:31 prometeu migração, e migração literal jogaria fora um dado. O que o k6 mede é *quantas tentativas o cliente fez*; o que o servidor mede é *quantas tentativas chegaram*. Sob 1000 VUs contra a pessimista, um VU desiste em `BID_DEADLINE` enquanto o servidor ainda segura a requisição: a tentativa existiu para o servidor e não para o cliente. A diferença entre as duas curvas é essa perda, e ela é informação, não ruído.

Então: a série do servidor fica com o nome publicado, `bid_attempts_per_accept`, e a do k6 passa a se chamar `client_attempts_per_accept`, saindo em `client.json` como `clientAttemptsPerAccept`. Duas medições com o mesmo nome em lugares diferentes seria a única forma garantida de alguém plotar a errada.

`cmd/checker` não lê nenhum dos dois campos — `clientReport` nunca teve `attemptsPerAccept` —, então a renomeação **não toca o checker**. É verificável com `git diff --name-only cmd/checker`, e é um dos aceites do C7.

### O que esta spec faz com os números já medidos

O middleware entra no caminho quente das três engines e custa dois round-trips de Redis por lance com chave. É constante entre as estratégias, então não move o cruzamento das curvas — mas move o **nível** das três.

Duas consequências, e as duas são registradas em vez de contornadas:

- Nenhum número medido antes desta spec pode aparecer no mesmo gráfico que um medido depois. As células `e2c5-pess` e `e2c5-otim` da spec 01 continuam válidas como o que eram — a prova de que a engine funciona ponta a ponta — e não como linha de base de latência. A matriz da etapa 5 roda inteira depois desta spec, de uma vez, como a decisão 13 já exigia por outro motivo
- Não entra variável de ambiente para desligar a idempotência. Ela já é desligável do lado certo: **não mandar o header**. Um lance sem chave não toca o Redis, então a célula de controle "sem idempotência" é uma escolha do gerador de carga e custa zero em configuração. Um `IDEMPOTENCY=on|off` seria um quinto eixo na matriz respondendo a uma pergunta que este projeto não fez

O `docker compose` passa a limitar CPU e memória do Redis, e `bench/env.sh` passa a gravar esses limites: o terceiro invariante de método diz recursos fixos e iguais, e o Redis acabou de entrar no caminho medido. Um serviço sem limite num host carregado é uma variável escondida dentro de um número publicado.

## Requisitos Funcionais

### RF01 - O middleware

`internal/idem` expõe `Middleware(store *Store, m Metrics, log *slog.Logger) gin.HandlerFunc`, montado em `POST /auctions/:id/bids` **depois** de `RequireUserID` e antes do handler.

1. Sem `X-Idempotency-Key`: `c.Next()` e mais nada. Nenhum comando ao Redis, nenhuma métrica, nenhum buffer de resposta
2. Chave presente e não parseável como UUID: `400` com `ErrorResponse{Error: "invalid_idempotency_key"}`, sem chamar a engine
3. Chave válida: o claim. `replay` devolve `201` com o corpo guardado e `X-Idempotency-Replayed: true`; `busy` devolve `425` com `ErrorResponse{Error: "idempotency_in_flight", Retryable: true}`; `go` segue para o handler
4. Erro de Redis em qualquer ponto: `503` com `ErrorResponse{Error: "unavailable", Retryable: true}` e uma linha de log
5. Na saída, em `defer`: `201` grava o corpo e observa `bid_attempts_per_accept`; qualquer outro desfecho apenas libera `busy`

O middleware não lê o corpo da requisição, não conhece `internal/bid` e não tem `switch` nenhum por estratégia.

### RF02 - A entrada no Redis

`internal/idem.Store` embrulha `*redis.Client` e expõe exatamente dois métodos: `Claim(ctx, key) (Verdict, error)` e `Finish(ctx, key, accepted bool, body []byte) error`.

`Claim` roda o script Lua acima com `redis.NewScript`, sob `idem:<uuid>`, com TTL em voo de 30s. `Finish` roda um pipeline: no aceite, `HSET done`, `HDEL busy` e `PEXPIRE` de 5min; fora dele, `HDEL busy` sozinho.

Nenhum outro comando de Redis existe neste pacote. `KEYS`, `SCAN` e `FLUSHDB` não aparecem em código de aplicação.

### RF03 - As duas séries

`internal/metrics` ganha `NewIdempotency(reg prometheus.Registerer, strategy string) idem.Metrics`, registrando `idempotency_hits_total{strategy,kind}` com os dois `kind` ligados no boot e `bid_attempts_per_accept{strategy}` com buckets lineares de 1 a 10.

`idem.Metrics` é uma struct de primitivas do Prometheus declarada em `internal/idem`: o middleware recebe os observadores prontos e nunca aprende um nome de série, exatamente como a engine pessimista recebe um `prometheus.Observer` (decisão 28). Todo nome de série continua morando em `internal/metrics`.

Duplicata barrada incrementa o `kind` que a barrou. Erro de Redis não incrementa nada.

### RF04 - A chave chega ao banco

`internal/bid/optimistic/sql.go` e `internal/bid/pessimistic/sql.go` passam a incluir `idempotency_key` no `INSERT`, com `NULLIF($n, '')`, alimentado por `req.IdempotencyKey` — o campo que a etapa 1 reservou e ninguém lia.

Nenhuma outra linha das engines muda. Em particular: nenhuma engine lê o Redis, nenhuma decide nada com a chave, e o desfecho de um lance não depende de ela existir.

`internal/bid/enginetest` ganha um caso de conformidade: aceite com chave grava aquela chave; aceite sem chave grava `NULL`. A assinatura de `RunConformance` **não muda**.

### RF05 - `/readyz` e o cliente de Redis

`internal/db` ganha `NewRedis(ctx, url string) (*redis.Client, error)`, que faz parse da URL, conecta e faz `PING` — o mesmo formato de `db.NewPool`, e no mesmo pacote, porque a etapa 4 vai precisar do mesmo cliente para os Streams.

`httpapi.Readyz` passa a receber uma lista de condições nomeadas em vez de dois parâmetros posicionais, e `cmd/auctiond` registra três: `database`, `schema` e `redis`. O corpo da resposta mantém o formato atual, nomeando a condição que falhou.

`/healthz` não muda.

### RF06 - O k6 manda a chave

`bench/bid-storm.js` passa a enviar `X-Idempotency-Key` em toda tentativa, com **uma chave por lance lógico**, reusada por todas as tentativas do laço de retentativa.

A chave é um UUID derivado de `(nonce da VU, __VU, __ITER)`, com o nonce sorteado no contexto de init. O nonce não é enfeite: `__VU` e `__ITER` recomeçam do zero entre o warmup e a corrida medida, e sem ele a corrida medida receberia como replay as respostas guardadas do warmup — uma célula inteira de `201` que nunca escreveu linha, e nada no relatório denunciando isso.

`bid_attempts_per_accept` do k6 é renomeada para `client_attempts_per_accept`, e o campo de `client.json` para `clientAttemptsPerAccept`. Nenhum outro campo de `client.json` muda de nome, de tipo ou de presença.

`bench/run-cell.sh` limpa o Redis dentro de `reset()`, junto do `TRUNCATE` e do `VACUUM ANALYZE`: a decisão 13 diz que cada célula parte do mesmo estado, e agora o estado do sistema inclui o Redis. As duas defesas — o nonce e o flush — cobrem coisas diferentes: o nonce protege quem roda o k6 na mão, o flush protege a matriz.

### RF07 - O que não muda

O diff não pode conter alteração em `cmd/checker`, `cmd/seed`, `migrations/`, `internal/store`, `internal/bid/engine.go`, `internal/bid/outcome.go`, `internal/httpapi/bids.go` nem `internal/httpapi/auctions.go`.

`internal/httpapi/bids.go` é o mais importante da lista: o handler de lance não pode ganhar uma linha por causa da idempotência. Se ele precisar, o middleware está no lugar errado.

## Requisitos Nao Funcionais

- **Uma dependência de runtime nova, e uma só:** `github.com/redis/go-redis/v9`. Em teste, `testcontainers-go/modules/redis`. `rueidis` foi descartado: o gargalo deste sistema não é o cliente de Redis, e a semântica do go-redis é a que um revisor consegue conferir de cabeça
- `internal/idem` importa `gin`, `go-redis`, `uuid` e `prometheus` — e **nada** de `internal/bid`, `internal/app`, `internal/store` ou `internal/metrics`
- Dois round-trips de Redis por lance com chave; zero sem chave. Nenhum caminho faz três
- O contêiner de Redis dos testes usa `redis:7-alpine`, a mesma imagem do compose
- A suíte de `internal/idem` roda em menos de 30 segundos, contêiner incluído
- `go vet ./...` limpo; `gofmt -l .` vazio
- Nenhum log por requisição, com a única exceção do `503` de Redis — a mesma exceção que o handler já tem
- O buffer de resposta só existe quando há chave, e guarda o corpo de um `201`, que tem ~150 bytes

## Budget do PR

Até 20 arquivos e aproximadamente 750 linhas de código próprio.

É mais que o dobro da spec 01, e a razão é que esta spec atravessa o sistema em vez de acrescentar uma peça ao lado das outras: um pacote novo, uma dependência nova, uma condição nova de readiness, duas séries, duas engines tocadas, o gerador de carga e o compose. Ainda assim o handler de lance não muda, e nenhuma engine aprende o que é uma chave.

Se o PR passar de 20 arquivos ou 750 linhas, **pare e reporte** em vez de continuar. E se a conta estourar por causa de `internal/httpapi`, pare mais cedo: é sinal de que a idempotência vazou para dentro do handler.

## Claude Code

- Modelo: `claude-opus-5`
- Esforco: alto
- Referencia permitida: `docs/projeto/api.md`, `docs/projeto/schema.md`, `docs/projeto/provas.md`, `docs/projeto/observabilidade.md`, `docs/projeto/arquitetura.md`, `docs/decisoes/etapa-1.md`, `docs/decisoes/etapa-2.md`, `docs/specs/etapa-2/01-spec-engine-pessimista.md`, `docs/specs/etapa-2/02-spec-idempotencia.md`

Prompt:

```text
Implemente docs/specs/etapa-2/02-spec-idempotencia.md no repositorio bid-storm.

Leia antes de comecar:
  docs/specs/etapa-2/02-spec-idempotencia.md  (a spec — a autoridade)
  docs/decisoes/etapa-2.md                    (o porque; decisoes 31 a 41)
  docs/decisoes/etapa-1.md                    (decisoes 2, 5, 9, 13, 15, 16,
                                               18, 21 e 23)
  internal/httpapi/identity.go                (o precedente de forma e tamanho
                                               de um middleware neste projeto)
  internal/metrics/lock.go                    (o precedente de como uma serie
                                               chega a quem a alimenta)
  bench/bid-storm.js                          (o apostador que vai mandar a chave)

ATENCAO: a spec emenda provas.md em dois pontos, e observabilidade.md em um:
  - a duplicata em voo recebe 425, NUNCA 409, e NUNCA espera (decisao 33).
    provas.md dizia "aguarda ou recebe 409 Idempotency-In-Flight"
  - a chave nomeia o LANCE LOGICO, nao a requisicao: ela atravessa as
    retentativas re-miradas, e por isso NAO existe impressao digital do corpo
    (decisao 31)
  - idempotency_hits_total nasce com os labels strategy e kind
    (decisao 37); observabilidade.md a declarava sem label

Escopo: apenas RF01..RF07. NAO implemente as duplicatas de 10% no k6, o
contador de replay do cliente, a retentativa sobre erro de transporte, o
invariante I7 do checker nem o aperto do I5 — sao todos da spec 03. NAO
implemente engine shard nem Redis Streams.

Regras:
- Modulo: github.com/samuka7abr/bid-storm
- NAO altere cmd/checker, cmd/seed, migrations/, internal/store,
  internal/bid/engine.go, internal/bid/outcome.go, internal/httpapi/bids.go
  nem internal/httpapi/auctions.go. Se algum deles parecer precisar de
  mudanca, pare e reporte.
- O handler de lance nao ganha uma linha. Se a idempotencia precisar entrar
  em internal/httpapi/bids.go, o middleware esta no lugar errado.
- A assinatura de enginetest.RunConformance nao muda. O caso novo e um t.Run
  usando a engine que a fabrica ja devolveu.
- Nenhum switch por nome de estrategia fora de internal/app. O middleware
  recebe o label pronto e nunca aprende qual engine esta atras dele.
- Uma unica dependencia de runtime nova: github.com/redis/go-redis/v9.
- Rode os checkpoints C1..C8 e cole a saida real de cada um. Nao declare
  aceite sem a saida do comando.
- Se estourar o budget de 20 arquivos / ~750 linhas, pare e reporte.
- Nao altere nada dentro de docs/.
```

## Arquivos Esperados

Criar:

```text
internal/idem/idem.go              o middleware e o embrulho do ResponseWriter
internal/idem/store.go             o script Lua, o claim e o finish
internal/idem/metrics.go           a struct que o middleware alimenta
internal/db/redis.go               NewRedis: parse, conexao e ping
internal/metrics/idempotency.go    os dois nomes de serie e os buckets
internal/testsupport/redis.go      Redis efemero por testcontainers, redis:7-alpine
```

Editar:

```text
internal/httpapi/router.go              monta o middleware entre a identidade e o handler
internal/httpapi/render.go              dois codigos de erro novos
internal/httpapi/health.go              Readyz com condicoes nomeadas
internal/app/engine.go                  ou internal/app/idem.go: constroi o middleware
cmd/auctiond/main.go                    cliente de Redis, terceira condicao, wiring
internal/bid/optimistic/sql.go          idempotency_key no INSERT
internal/bid/pessimistic/sql.go         idempotency_key no INSERT
bench/bid-storm.js                      a chave por lance logico e a renomeacao da Trend
bench/run-cell.sh                       o flush do Redis dentro de reset()
bench/env.sh                            os limites do Redis no env.json
docker-compose.yaml                     cpus e mem_limit no servico redis
```

`internal/idem/metrics.go` existe para que `internal/metrics` possa devolver uma struct tipada sem que `internal/idem` precise importar `internal/metrics` — a dependência aponta numa direção só, como já acontece entre `internal/metrics` e `internal/bid`.

## Testes

Adicionar:

```text
internal/idem/idem_test.go        os sete caminhos do middleware, contra um Redis real
internal/idem/store_test.go       os tres veredictos do script, os dois TTLs, o finish
internal/metrics/idempotency_test.go  nomes, labels, buckets e os filhos ligados no boot
```

Editar:

```text
internal/httpapi/health_test.go        a terceira condicao, falhando uma de cada vez
internal/bid/enginetest/conformance.go o caso da chave gravada (t.Run novo, fabrica igual)
```

Os sete caminhos do middleware, explicitamente: sem chave, chave malformada, primeira tentativa, replay de um aceite, duplicata em voo, retentativa re-mirada sob a mesma chave, e Redis fora do ar. O caso da duplicata em voo bloqueia o handler de teste num canal — é a única forma de ter duas requisições com a mesma chave dentro do middleware ao mesmo tempo, e é o caso que provas.md chama de "o interessante".

## Checkpoints Mensuraveis

### C1 - Sem chave, nada muda

```bash
make up && sleep 15
make run STRATEGY=optimistic && sleep 10

make seed AUCTIONS=1 TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export UID=$(uuidgen)

docker compose exec -T redis redis-cli FLUSHALL
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H 'Content-Type: application/json' \
  -d '{"amountCents":100,"expectedVersion":0}'

docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT count(*), count(idempotency_key) FROM bids;"
docker compose exec -T redis redis-cli DBSIZE
curl -s localhost:8080/metrics | grep -E 'idempotency_hits_total|bid_attempts_per_accept_count'
```

Aceite:

- `201`, com o mesmo corpo que a etapa 1 devolvia
- `1|0`: uma linha, e a chave `NULL` — a coluna continua fora do índice parcial
- `DBSIZE` devolve `0`: um lance sem chave **não toca o Redis**
- `/metrics` já traz as duas séries com os dois `kind` em zero e o histograma com `count 0`: os filhos são ligados no boot, e zero é uma afirmação diferente de silêncio

### C2 - Com chave, a entrada nasce e a coluna é escrita

```bash
export KEY=$(uuidgen)
curl -si -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY" \
  -d '{"amountCents":200,"expectedVersion":1}' | tee /tmp/c2.txt | head -12

docker compose exec -T redis redis-cli HGETALL "idem:$KEY"
docker compose exec -T redis redis-cli PTTL  "idem:$KEY"
docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT idempotency_key FROM bids ORDER BY seq DESC LIMIT 1;"
```

Aceite:

- `201`, **sem** o header `X-Idempotency-Replayed`
- `HGETALL` mostra `attempts 1`, `done` com o corpo do `201`, e **nenhum** `busy`
- `PTTL` entre 240000 e 300000: o finish promoveu o TTL de 30s para 5min
- A coluna traz exatamente `$KEY`

### C3 - Reenvio devolve o mesmo `201`, byte a byte

```bash
curl -si -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY" \
  -d '{"amountCents":200,"expectedVersion":1}' | tee /tmp/c3.txt | head -12

diff <(tail -1 /tmp/c2.txt) <(tail -1 /tmp/c3.txt) && echo 'corpos identicos'
docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT count(*) FROM bids;"
curl -s localhost:8080/metrics | grep idempotency_hits_total
```

Aceite:

- `201` com `X-Idempotency-Replayed: true`
- Os dois corpos são idênticos: mesmo `bidId`, mesmo `seq`, mesmo `currentVersion`. O `diff` sai vazio
- `bids` continua com o mesmo número de linhas: **zero lances duplos**, que é a promessa de provas.md §3
- `idempotency_hits_total{kind="replayed"}` vale `1`; `kind="in_flight"` continua `0`

### C4 - Retentativa re-mirada não é duplicata

É a decisão 31 executando. A mesma chave, corpo diferente, e o lance **passa**.

```bash
export KEY2=$(uuidgen)
# abaixo do minimo: 422
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY2" \
  -d '{"amountCents":1,"expectedVersion":2}'
docker compose exec -T redis redis-cli HGETALL "idem:$KEY2"

# re-mira: MESMA chave, outro corpo
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY2" \
  -d '{"amountCents":300,"expectedVersion":2}'

curl -s localhost:8080/metrics | grep -E 'bid_attempts_per_accept_(sum|count)'
curl -s localhost:8080/metrics | grep idempotency_hits_total
```

Aceite:

- `422` e depois `201` — a segunda tentativa **não** é barrada, e não recebe o `422` guardado: rejeição não guarda nada
- Entre as duas, o hash tem `attempts 1` e nenhum `busy`: o finish da rejeição liberou a marca e preservou a contagem
- `bid_attempts_per_accept_count` vale `1` e `_sum` vale `2`: o servidor mediu duas tentativas para um aceite, que é o fio que o k6 detinha sozinho até aqui
- `idempotency_hits_total` não se moveu: uma retentativa não é uma duplicata barrada

### C5 - Duplicata em voo: `425`, na hora

O lock externo da spec 01 serve de novo, agora para segurar a **primeira** requisição enquanto a segunda chega.

```bash
make run STRATEGY=pessimistic && sleep 10
make seed AUCTIONS=1 TRUNCATE=1
export AID=$(jq -r '.[0].id' bench/auctions.json)
export KEY3=$(uuidgen)

docker compose exec -T postgres psql -U auction -d auction -c \
  "BEGIN; SELECT id FROM auctions WHERE id='$AID' FOR UPDATE; SELECT pg_sleep(4); COMMIT;" &
sleep 1

curl -s -o /tmp/first.json -w 'primeira: %{http_code} em %{time_total}s\n' \
  -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY3" -d '{"amountCents":500}' &
sleep 1

curl -si -w '\nsegunda em %{time_total}s\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $KEY3" -d '{"amountCents":500}' | head -14
wait

curl -s localhost:8080/metrics | grep idempotency_hits_total
docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT count(*) FROM bids WHERE idempotency_key = '$KEY3';"
```

Aceite:

- A segunda volta `425` com `idempotency_in_flight` e `retryable: true`, e **em milissegundos** — não nos ~2s que faltavam para o lock ser liberado. É essa diferença de tempo que separa "não espera" de "espera", e é ela que a decisão 33 exige
- A primeira volta `201` depois de ~3s, e a resposta dela não traz `X-Idempotency-Replayed`
- Exatamente **uma** linha com `$KEY3`
- `idempotency_hits_total{kind="in_flight"}` vale `1`
- O `425` não carrega `currentVersion`, `currentHighestBid` nem `minNextBid`: nenhuma engine foi consultada, então não há estado a publicar

### C6 - Redis fora do ar falha fechado, e sem chave continua passando

```bash
docker compose stop redis && sleep 2

curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H "X-Idempotency-Key: $(uuidgen)" -d '{"amountCents":700}'
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -d '{"amountCents":800}'

curl -s -w '\n%{http_code}\n' localhost:8080/readyz | jq -c
curl -s -w '\n%{http_code}\n' localhost:8080/healthz
curl -s localhost:8080/metrics | grep idempotency_hits_total

docker compose start redis && sleep 8
curl -s -w '\n%{http_code}\n' localhost:8080/readyz | jq -c
```

Aceite:

- Com chave: `503` com `unavailable` e `retryable: true`, e uma linha de log no `auctiond`
- **Sem chave: `201`.** Um lance sem chave nunca toca o Redis, e é isso que faz a célula de controle da decisão 41 existir sem variável de ambiente nenhuma
- `/readyz` responde `503` nomeando `redis`; `/healthz` continua `200`
- `idempotency_hits_total` não se moveu em nenhum `kind`: Redis caído não barra duplicata alguma, e contar erro como hit seria dizer o contrário
- Depois do `start`, `/readyz` volta a `200` sem reiniciar o `auctiond`

### C7 - Testes, e o que não foi tocado

```bash
go test ./internal/idem/... -race -count=1 -v
go test ./internal/bid/... -race -count=1
go test ./... -race -count=1
gofmt -l . && go vet ./...
git diff --name-only cmd/checker cmd/seed migrations/ internal/store \
  internal/httpapi/bids.go internal/bid/engine.go internal/bid/outcome.go
```

Aceite:

- Os sete caminhos do middleware passam com `-race`, contra um Redis de contêiner em `redis:7-alpine`
- A conformidade passa nas **duas** engines, incluindo o caso novo da chave gravada, sem mudança na assinatura de `RunConformance`
- Toda a suíte passa; `gofmt -l .` vazio e `go vet` sem saída
- O `git diff --name-only` não lista nenhum arquivo: RF07 cumprido, e em particular o handler de lance não ganhou uma linha

### C8 - Duas células reais, com o fio funcionando

```bash
make run STRATEGY=optimistic && sleep 10
make bench RUN=e2c8-otim STRATEGY=optimistic AUCTIONS=1 POLICY=immediate SCENARIO=ramp
curl -s localhost:8080/metrics | grep -E 'bid_attempts_per_accept_(count|sum)|bid_outcomes_total.*accepted'

make run STRATEGY=pessimistic && sleep 10
make bench RUN=e2c8-pess STRATEGY=pessimistic AUCTIONS=1 POLICY=immediate SCENARIO=ramp

jq '{accepted,conflict,outbid,exhausted,clientAttemptsPerAccept}' bench/results/e2c8-otim/client.json
jq '.limits' bench/results/e2c8-otim/env.json
docker compose exec -T postgres psql -U auction -d auction -tAc \
  "SELECT count(*), count(idempotency_key), count(DISTINCT idempotency_key) FROM bids;"
cat bench/results/e2c8-otim/checker.txt
cat bench/results/e2c8-pess/checker.txt
```

Aceite:

- As duas células terminam com código 0 e os seis invariantes verdes, sem uma linha de mudança em `cmd/checker`
- As três contagens do `psql` são **iguais**: toda linha tem chave, e nenhuma chave se repete. A rede embaixo nunca precisou disparar, e agora existe
- `bid_attempts_per_accept_count` bate com `bid_outcomes_total{outcome="accepted"}` do mesmo processo: todo aceite virou amostra, nenhum ficou de fora
- `clientAttemptsPerAccept` sai em `client.json` com o nome novo, e fica **próximo** do histograma do servidor sem ser igual — a diferença é a decisão 38, não um bug
- `idempotency_hits_total` fica em **zero** nas duas células: nesta spec o k6 manda a chave e não manda duplicata. É a spec 03 que tira esse número do zero, e é assim que se sabe que o número dela veio da injeção e não de ruído
- `env.json` grava os limites do Redis junto dos outros três

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
  -d '{"title":"Smoke idempotencia","startingBidCents":0,"minIncrementCents":100,
       "endsAt":"'$(date -u -d '+5 min' +%Y-%m-%dT%H:%M:%SZ)'"}' | jq

export AID=<id devolvido>
export UID=$(uuidgen)
export K=$(uuidgen)

# tres vezes a mesma requisicao, com a mesma chave
for i in 1 2 3; do
  curl -s -o /tmp/s$i.json -w "%{http_code} " -X POST localhost:8080/auctions/$AID/bids \
    -H "X-User-Id: $UID" -H "X-Idempotency-Key: $K" -d '{"amountCents":100}'
done; echo
diff /tmp/s1.json /tmp/s2.json && diff /tmp/s2.json /tmp/s3.json && echo 'as tres iguais'

# chave torta
curl -s -w '\n%{http_code}\n' -X POST localhost:8080/auctions/$AID/bids \
  -H "X-User-Id: $UID" -H 'X-Idempotency-Key: nao-e-uuid' -d '{"amountCents":900}'

curl -s localhost:8080/auctions/$AID | jq
curl -s localhost:8080/metrics | grep -E 'idempotency_hits_total|bid_attempts_per_accept'

make down
```

Aceite manual:

- As três respostas são `201` e idênticas; o leilão fica em `version: 1`, com `currentHighestBid: 100`
- A chave torta devolve `400 invalid_idempotency_key`, e o leilão continua em `version: 1`
- `idempotency_hits_total{kind="replayed"}` vale `2`
- `bid_attempts_per_accept_count` vale `1`, com `_sum` `1`: um aceite, uma tentativa. Os dois replays não são tentativas
- `make down` derruba tudo sem contêiner órfão

## Definicao De Pronto

- RF01 a RF07 implementados
- C1 a C8 executados, com a saída real colada no PR — checkpoint sem saída não conta como aceito
- `RunConformance` passando nas duas engines com o caso novo, **sem mudança na assinatura**
- `git diff --name-only` vazio para `cmd/checker`, `cmd/seed`, `migrations/`, `internal/store`, `internal/httpapi/bids.go`, `internal/bid/engine.go` e `internal/bid/outcome.go`
- Uma única dependência de runtime nova em `go.mod`
- Todos os testes passando com `-race`; `go vet ./...` limpo e `gofmt -l .` vazio
- Budget respeitado, ou desvio reportado antes de estourar
- Nenhum arquivo dentro de `docs/` alterado
- Nada de duplicata injetada no k6, contador de replay no cliente, I7, aperto do I5, engine shard ou Redis Streams no diff
