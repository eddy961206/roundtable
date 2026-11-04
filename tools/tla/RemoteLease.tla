---------------------------- MODULE RemoteLease ----------------------------
\* covers: internal/room/remote.go internal/worker/worker.go
\* reach: turn = "done"
\* reach: turn = "done" /\ execs = 2
\* reach: gen = 2 /\ turn = "done"
(* One remote seat's turn handed to worker processes (community worker).

   Host (internal/room/remote.go), every step under h.mu:
     runRemote offers the turn in the seat's slot (job, claimed = FALSE).
     claim gives it to a polling worker unless another worker holds a lease
     that is still fresh (beat within remoteLease, 15 s). RemoteProgress and
     RemoteFinish take reports only from the holder (sl.by) with a valid
     token; a finish hands the result to runRemote and clears the slot.
     runRemote ends the turn on the result, on its own timeout or cancel
     (drop), or on the claim timeout (2 min) while nobody claimed it.
   Worker (internal/worker/worker.go): a process with a random id
   (X-RT-Worker) long-polls RemoteNext, runs the CLI, reports progress every
   2 s, then delivers the result. A 4xx means the turn is gone; a network
   failure or 5xx keeps the result and retries. A restarted process gets a new
   id. A long poll waits outside h.mu and checks the token again on waking.
   An operator can re-issue the seat token; worker b then runs with the new one.

   Time is not modeled. Expire is the 15 s without a beat after which
   another worker may take the lease.

   Flags turn off one guard each, as the code was before a fix:
     RecheckToken  d69d988: a poll that waited with a revoked token claimed.
     HonorLease    b93c27c: two processes with one token both ran the turn.
     RetryDeliver  d69d988: a failed upload dropped the result and polled again,
                   got the same claimed turn back and ran it a second time.
     AtomicTimeout runRemote read "not claimed" under h.mu, unlocked, then
                   dropped the job under h.mu again; a poll in between took
                   the turn the host then ended as never taken. *)
EXTENDS Naturals

CONSTANTS Workers, MaxEpoch, MaxExpire, MaxLoss, RecheckToken, HonorLease, RetryDeliver, AtomicTimeout

ASSUME RecheckToken \in BOOLEAN /\ HonorLease \in BOOLEAN /\ RetryDeliver \in BOOLEAN /\ AtomicTimeout \in BOOLEAN

VARIABLES
    turn,       \* runRemote: "none", "waiting", "done", "unclaimed", "error", "cancelled"
    timer,      \* the claim timeout: "armed", "checked" (read "not claimed", h.mu released)
    job,        \* sl.job != nil
    claimed,    \* sl.claimed
    by,         \* sl.by: <<worker, epoch>> of the holder, or <<"-", 0>>
    fresh,      \* now - sl.beat < remoteLease
    applied,    \* results runRemote took
    gen,        \* current token generation
    tok,        \* token generation each worker runs with
    ws,         \* worker state: "poll", "wait", "run", "deliver", "off"
    epoch,      \* process restarts of each worker (its id)
    execs,      \* CLI runs of this turn, on any worker
    expires, losses
vars == <<turn, timer, job, claimed, by, fresh, applied, gen, tok, ws, epoch, execs, expires, losses>>

Nobody == <<"-", 0>>
Id(w) == <<w, epoch[w]>>
Valid(w) == tok[w] = gen
Holds(w) == job /\ claimed /\ by = Id(w) /\ Valid(w)
CanClaim(w) == job /\ (~claimed \/ by = Id(w) \/ ~fresh \/ ~HonorLease)
Ended == turn \in {"done", "unclaimed", "error", "cancelled"}

Init ==
    /\ turn = "none" /\ timer = "armed" /\ job = FALSE /\ claimed = FALSE /\ by = Nobody /\ fresh = FALSE
    /\ applied = 0 /\ gen = 1 /\ tok = [w \in Workers |-> 1]
    /\ ws = [w \in Workers |-> "poll"] /\ epoch = [w \in Workers |-> 0]
    /\ execs = 0 /\ expires = 0 /\ losses = 0

-------------------------------------------------------------------------------
\* Host

Offer ==
    /\ turn = "none"
    /\ turn' = "waiting" /\ job' = TRUE /\ claimed' = FALSE
    /\ UNCHANGED <<timer, by, fresh, applied, gen, tok, ws, epoch, execs, expires, losses>>

\* The claim timeout fires and finds nobody holding the turn. With
\* AtomicTimeout the drop happens under the same h.mu hold.
ClaimTimeout ==
    /\ turn = "waiting" /\ timer = "armed" /\ job /\ ~claimed
    /\ IF AtomicTimeout
       THEN turn' = "unclaimed" /\ job' = FALSE /\ UNCHANGED timer
       ELSE timer' = "checked" /\ UNCHANGED <<turn, job>>
    /\ UNCHANGED <<claimed, by, fresh, applied, gen, tok, ws, epoch, execs, expires, losses>>

\* drop() after h.mu was released: the job goes whoever took it meanwhile.
TimeoutDrop ==
    /\ turn = "waiting" /\ timer = "checked"
    /\ turn' = "unclaimed" /\ job' = FALSE
    /\ UNCHANGED <<timer, claimed, by, fresh, applied, gen, tok, ws, epoch, execs, expires, losses>>

\* The turn's own timeout, or the person stops it: drop().
Stop ==
    /\ turn = "waiting"
    /\ \E end \in {"error", "cancelled"} : turn' = end
    /\ job' = FALSE
    /\ UNCHANGED <<timer, claimed, by, fresh, applied, gen, tok, ws, epoch, execs, expires, losses>>

\* 15 s pass without a beat from the holder.
Expire ==
    /\ job /\ claimed /\ fresh /\ expires < MaxExpire
    /\ fresh' = FALSE /\ expires' = expires + 1
    /\ UNCHANGED <<turn, timer, job, claimed, by, applied, gen, tok, ws, epoch, execs, losses>>

\* The operator re-issues the seat token and restarts worker b with it.
Reissue ==
    /\ gen = 1 /\ "b" \in Workers
    /\ gen' = 2 /\ tok' = [tok EXCEPT !["b"] = 2]
    /\ ws' = [ws EXCEPT !["b"] = "poll"] /\ epoch' = [epoch EXCEPT !["b"] = epoch["b"] + 1]
    /\ epoch["b"] < MaxEpoch
    /\ UNCHANGED <<timer, turn, job, claimed, by, fresh, applied, execs, expires, losses>>

-------------------------------------------------------------------------------
\* Worker

Claim(w) ==
    /\ claimed' = TRUE /\ by' = Id(w) /\ fresh' = TRUE
    /\ ws' = [ws EXCEPT ![w] = "run"] /\ execs' = execs + 1

\* RemoteNext before it waits: a revoked token is refused (401: the worker stops).
Poll(w) ==
    /\ ws[w] = "poll"
    /\ IF ~Valid(w) THEN ws' = [ws EXCEPT ![w] = "off"] /\ UNCHANGED <<claimed, by, fresh, execs>>
       ELSE IF CanClaim(w) THEN Claim(w)
       ELSE ws' = [ws EXCEPT ![w] = "wait"] /\ UNCHANGED <<claimed, by, fresh, execs>>
    /\ UNCHANGED <<timer, turn, job, applied, gen, tok, epoch, expires, losses>>

\* The long poll wakes (a new job, the wait ending) and takes h.mu again.
Wake(w) ==
    /\ ws[w] = "wait"
    /\ IF RecheckToken /\ ~Valid(w) THEN ws' = [ws EXCEPT ![w] = "off"] /\ UNCHANGED <<claimed, by, fresh, execs>>
       ELSE IF CanClaim(w) THEN Claim(w)
       ELSE ws' = [ws EXCEPT ![w] = "poll"] /\ UNCHANGED <<claimed, by, fresh, execs>>
    /\ UNCHANGED <<timer, turn, job, applied, gen, tok, epoch, expires, losses>>

\* A progress report: the holder keeps its lease; anyone else stops its CLI.
Beat(w) ==
    /\ ws[w] = "run"
    /\ IF Holds(w) THEN fresh' = TRUE /\ UNCHANGED ws
       ELSE ws' = [ws EXCEPT ![w] = "poll"] /\ UNCHANGED fresh
    /\ UNCHANGED <<timer, turn, job, claimed, by, applied, gen, tok, epoch, execs, expires, losses>>

Complete(w) ==
    /\ ws[w] = "run"
    /\ ws' = [ws EXCEPT ![w] = "deliver"]
    /\ UNCHANGED <<timer, turn, job, claimed, by, fresh, applied, gen, tok, epoch, execs, expires, losses>>

\* One upload. "acked": the host answered. "lostAck": the host took it but the
\* answer was lost. "lost": it never reached the host (network, 5xx).
Deliver(w) ==
    /\ ws[w] = "deliver"
    /\ \E outcome \in {"acked", "lostAck", "lost"} :
        /\ (outcome # "acked") => losses < MaxLoss
        /\ losses' = IF outcome = "acked" THEN losses ELSE losses + 1
        /\ IF outcome = "lost"
           THEN /\ ws' = [ws EXCEPT ![w] = IF RetryDeliver THEN "deliver" ELSE "poll"]
                /\ UNCHANGED <<turn, job, applied>>
           ELSE IF Holds(w) /\ turn = "waiting"
           THEN /\ applied' = applied + 1 /\ job' = FALSE /\ turn' = "done"
                /\ ws' = [ws EXCEPT ![w] = IF outcome = "acked" THEN "poll" ELSE "deliver"]
           ELSE /\ ws' = [ws EXCEPT ![w] = "poll"]   \* 4xx: the turn is gone here
                /\ UNCHANGED <<turn, job, applied>>
    /\ UNCHANGED <<timer, claimed, by, fresh, gen, tok, epoch, execs, expires>>

\* The worker process restarts and comes back with a new id.
Restart(w) ==
    /\ ws[w] # "off" /\ epoch[w] < MaxEpoch
    /\ ws' = [ws EXCEPT ![w] = "poll"] /\ epoch' = [epoch EXCEPT ![w] = epoch[w] + 1]
    /\ UNCHANGED <<timer, turn, job, claimed, by, fresh, applied, gen, tok, execs, expires, losses>>

Worker(w) == Poll(w) \/ Wake(w) \/ Beat(w) \/ Complete(w) \/ Deliver(w) \/ Restart(w)

Halt == ~ENABLED (Offer \/ ClaimTimeout \/ TimeoutDrop \/ Stop \/ Expire \/ Reissue \/ \E w \in Workers : Worker(w)) /\ UNCHANGED vars

Next == Offer \/ ClaimTimeout \/ TimeoutDrop \/ Stop \/ Expire \/ Reissue \/ (\E w \in Workers : Worker(w)) \/ Halt

Spec == Init /\ [][Next]_vars /\ WF_vars(Stop) /\ WF_vars(TimeoutDrop) /\ \A w \in Workers : WF_vars(Poll(w) \/ Wake(w) \/ Complete(w) \/ Deliver(w))

-------------------------------------------------------------------------------
\* Properties

TypeOK ==
    /\ turn \in {"none", "waiting", "done", "unclaimed", "error", "cancelled"}
    /\ ws \in [Workers -> {"poll", "wait", "run", "deliver", "off"}]

\* The turn takes one result at most.
AppliedOnce == applied <= 1

\* An ended turn leaves no job a worker could still claim.
EndedClearsSlot == Ended => ~job

\* A turn the host ends as never taken was not run by any worker: the error
\* says so, and the person sends the messages again.
UnclaimedNeverRan == turn = "unclaimed" => execs = 0

\* The CLI runs the turn a second time only after a holder went 15 s without a
\* beat (a frozen or restarted process). This is the lease's known cost: in
\* write mode the first run's changes may already be on that PC's disk.
RunsTwiceOnlyAfterExpiry == execs <= 1 \/ expires > 0

\* A worker never starts the CLI with a token that was re-issued away.
ClaimsOnlyWithValidToken ==
    [][\A w \in Workers : (ws[w] # "run" /\ ws'[w] = "run") => tok[w] = gen]_vars

\* Every offered turn ends.
TurnEnds == (turn = "waiting") ~> Ended

=============================================================================
