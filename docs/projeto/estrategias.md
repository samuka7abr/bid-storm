# As três estratégias

[← índice](../README.md)

Selecionadas por variável de ambiente (`BID_STRATEGY=optimistic|pessimistic|shard`), com a mesma interface e a mesma suíte de conformidade rodando contra as três.

---

## A interface

```go
type BidEngine interface {
    // Retorna somente quando o lance está DURÁVEL. Vale para as três engines.
    PlaceBid(ctx context.Context, req BidRequest) (BidResult, error)
}
```

Duas decisões estão codificadas nessa assinatura, e elas carregam o projeto inteiro.

**Rejeição de lance não é erro.** "Você perdeu" é uma resposta bem-sucedida da engine. `error` significa apenas *não consegui decidir*: Postgres caiu, contexto cancelado. O desfecho vem tipado no resultado, e o handler HTTP vira uma tabela de mapeamento — sem `errors.As` no caminho mais quente do sistema, e sem ganhar um `case` novo a cada engine.

**O resultado carrega o estado atual mesmo quando rejeita.** É desse estado que o handler monta o envelope uniforme de resposta, que é o que permite ao cliente retentar em um único round-trip.

```go
type Outcome uint8

const (
    Accepted Outcome = iota // 201
    Conflict                // 409 — versão desatualizada (só a otimista produz)
    TooLow                  // 422 — alguém passou na frente
    Closed                  // 410 — passou de ends_at, ou status já materializado
    NotFound                // 404
)

type BidRequest struct {
    AuctionID      uuid.UUID
    UserID         uuid.UUID
    AmountCents    int64
    ExpectedVersion *int64 // nil = engine ignora (pessimista e shard)
    IdempotencyKey  string // etapa 2
}

type AuctionState struct {
    Version           int64
    HighestBidCents   int64
    MinIncrementCents int64
    Status            Status
    EndsAt            time.Time
}

// Regra do servidor, nunca do cliente: se cada cliente escolhesse o próprio
// incremento, dois VUs competiriam sob regras diferentes.
func (s AuctionState) MinNextBid() int64 {
    return s.HighestBidCents + s.MinIncrementCents
}

type BidResult struct {
    Outcome Outcome
    Seq     int64        // preenchido se Accepted
    BidID   uuid.UUID    // preenchido se Accepted
    Current AuctionState // SEMPRE preenchido
}
```

---

## Otimista

O `UPDATE` condicional é uma linha. O que faz a métrica valer alguma coisa é o que acontece quando ele não afeta nada.

`RowsAffected() == 0` pode significar quatro coisas incompatíveis: versão desatualizada, lance baixo demais, leilão fechado, leilão inexistente. Tratar as quatro como `ErrConflict` destrói a métrica central da tese — um lance de R$ 5 num leilão de R$ 9.000 contaria como conflito de concorrência, e o cliente retentaria para sempre um lance que nunca passaria. Por isso o caminho de falha paga **um round-trip a mais para classificar**, e o caminho feliz não paga nada.

```go
func (e *Optimistic) PlaceBid(ctx context.Context, req BidRequest) (BidResult, error) {
    tx, err := e.pool.Begin(ctx)
    if err != nil {
        return BidResult{}, err
    }
    defer tx.Rollback(ctx)

    var seq int64
    err = tx.QueryRow(ctx, `
        UPDATE auctions
           SET highest_bid_cents = $1,
               highest_bidder    = $2,
               version           = version + 1
         WHERE id      = $3
           AND version = $4
           AND status  = 'open'
           AND now() < ends_at
           AND highest_bid_cents + min_increment_cents <= $1
        RETURNING version`,
        req.AmountCents, req.UserID, req.AuctionID, *req.ExpectedVersion,
    ).Scan(&seq)

    switch {
    case errors.Is(err, pgx.ErrNoRows):
        return e.classify(ctx, tx, req) // só aqui custa o round-trip extra
    case err != nil:
        return BidResult{}, err
    }

    bidID := uuid.New()
    if _, err = tx.Exec(ctx, `
        INSERT INTO bids (id, auction_id, user_id, amount_cents, seq, idempotency_key)
        VALUES ($1, $2, $3, $4, $5, $6)`,
        bidID, req.AuctionID, req.UserID, req.AmountCents, seq, req.IdempotencyKey,
    ); err != nil {
        return BidResult{}, err
    }

    // 201 só existe depois deste commit.
    if err = tx.Commit(ctx); err != nil {
        return BidResult{}, err
    }
    return BidResult{Outcome: Accepted, Seq: seq, BidID: bidID, Current: /* ... */}, nil
}

func (e *Optimistic) classify(ctx context.Context, tx pgx.Tx, req BidRequest) (BidResult, error) {
    var st AuctionState
    err := tx.QueryRow(ctx, `
        SELECT version, highest_bid_cents, min_increment_cents, status, ends_at
          FROM auctions WHERE id = $1`, req.AuctionID).Scan(/* ... */)

    switch {
    case errors.Is(err, pgx.ErrNoRows):
        return BidResult{Outcome: NotFound}, nil
    case err != nil:
        return BidResult{}, err
    case st.Status != Open || !time.Now().Before(st.EndsAt):
        return BidResult{Outcome: Closed, Current: st}, nil
    case req.AmountCents < st.MinNextBid():
        return BidResult{Outcome: TooLow, Current: st}, nil
    default:
        return BidResult{Outcome: Conflict, Current: st}, nil
    }
}
```

Métrica-chave: **taxa de conflito** e **número médio de tentativas por lance aceito**.

---

## Pessimista

```go
row := tx.QueryRow(ctx, `
    SELECT version, highest_bid_cents, min_increment_cents, status, ends_at
      FROM auctions WHERE id = $1 FOR UPDATE`, req.AuctionID)
// valida, escreve, commita
```

Ignora `ExpectedVersion`: o lock já garante que o estado lido é o vigente. Nunca produz `Conflict` — o perdedor recebe `TooLow`, que é o mesmo evento visto por outro mecanismo.

Métrica-chave: **tempo de espera em lock** e **saturação do pool de conexões**.

---

## Single-writer

Cada leilão é mapeado deterministicamente para um shard. O shard é dono do estado em memória e é o único que escreve.

```go
type shard struct {
    inbox    chan bidCmd
    auctions map[uuid.UUID]*auctionState // só esta goroutine toca aqui
}

func shardFor(auctionID uuid.UUID, n int) int {
    return int(binary.BigEndian.Uint64(auctionID[:8])) % n
}
```

A decisão que define esta engine não é o canal, é **quando ela responde**.

O caminho tentador é confirmar em memória e persistir depois — `p95` de 12ms contra 890ms do otimista, e um GIF impressionante. Mas aí parte da vitória não vem de eliminar contenção, vem de ter feito uma **promessa mais fraca**: o invariante *"todo lance que recebeu 201 está no banco"* seria literalmente falso se o processo morresse com o journal cheio, e comparar essa latência com a das outras duas seria comparar coisas diferentes.

Então o shard responde **depois do commit**, como as outras duas. O ganho vem de outro lugar, e é um ganho maior: como um único escritor detém o leilão, ele pode agrupar centenas de lances em **um** commit, amortizando o `fsync` em vez de disputar a linha.

```go
func (s *shard) run(ctx context.Context) {
    var batch []bidCmd
    flush := time.NewTicker(2 * time.Millisecond)

    for {
        select {
        case cmd := <-s.inbox:
            st := s.auctions[cmd.AuctionID]

            if !time.Now().Before(st.EndsAt) {
                cmd.reply <- BidResult{Outcome: Closed, Current: st.snapshot()}
                continue
            }
            if cmd.Amount < st.MinNextBid() {
                cmd.reply <- BidResult{Outcome: TooLow, Current: st.snapshot()}
                continue
            }

            // decidido na hora, respondido só depois de durável
            st.HighestBid, st.HighestBidder = cmd.Amount, cmd.UserID
            st.Seq++
            cmd.seq = st.Seq
            observeAccept(cmd)              // bid_accept_duration_seconds
            batch = append(batch, cmd)

            if len(batch) >= 256 {
                s.commit(ctx, batch)        // 1 fsync para 256 lances
                batch = batch[:0]
            }

        case <-flush.C:
            if len(batch) > 0 {
                s.commit(ctx, batch)
                batch = batch[:0]
            }
        }
    }
}

// commit persiste o lote e SÓ ENTÃO libera as respostas
func (s *shard) commit(ctx context.Context, batch []bidCmd) {
    if err := s.persist(ctx, batch); err != nil {
        for _, c := range batch {
            c.reply <- BidResult{} // erro de infra, o handler devolve 503
        }
        return
    }
    for _, c := range batch {
        c.reply <- BidResult{Outcome: Accepted, Seq: c.seq /* ... */}
    }
}
```

O gap entre `bid_accept_duration_seconds` (decidido em memória) e `bid_confirm_duration_seconds` (durável) passa a ser exatamente **o custo da durabilidade, exposto como métrica** em vez de escondido no contrato.

Métrica-chave: **latência de confirmação**, **profundidade do inbox** e **tamanho do lote por commit**.
