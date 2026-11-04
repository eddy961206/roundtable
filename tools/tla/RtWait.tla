------------------------------- MODULE RtWait -------------------------------
(* rt wait (internal/rtmcp/wait.go) against one room's turn state machine.

   Room (internal/room/hub.go), every step under h.mu, one seat:
     queued --Start--> running --Finish--> idle
     Start appends the seat reply as "streaming". Finish marks it "done" and,
     in the same critical section, may append a system notice (the input-token
     warning of budget.go usageNotices). A turn can also be queued without a
     message (a discussion start, a replayed turn) and leave the queue without
     an answer (CancelTurn, CancelRequest, holdTurn).
     A post to the running seat is steered into its turn (live) and queues
     nothing. The turn's reply, an older seq, answers it. A steer the turn did
     not take loses live and queues a new turn when the turn ends.
     A finished part reply can start a work step outside h.mu (submit check,
     merge: work.go) that later queues the review turn or stops.
   The room-list SSE feed is signaled when a step leaves the room summary
   (active, message count) different from the last published one (listed:
   hub.go publishList). PublishWork = FALSE is the server before the
   publishList calls: a reply that starts a work step published the summary
   without the step, so the step's end could leave the summary unchanged.

   Waiter: the feed holds at most one pending signal (subscribeRooms).
   readyRooms reads GET /api/rooms (ListRead), then, in a separate request,
   the room snapshot with its messages (MsgRead). An idle room whose list
   count equals the cached one (checked) is not read again.

   Done chooses the waiter's version:
     "window"  - before 5d6506c: a done seat reply among the messages the
                 max_chars budget kept (newest first); checked is set at
                 ListRead.
     "replied" - 5d6506c: a done seat reply above after_seq, kept or not;
                 checked is set at ListRead.
     "idle"    - b3a7b76: a done seat reply above after_seq and no running or
                 queued turn in the same snapshot; checked is set only when
                 that snapshot was idle. Work steps do not count as active.
     "current" - the contract: Answered and not Busy in the same snapshot,
                 where Busy includes work steps and Answered includes the reply
                 of a turn that took a steer at or after after_seq.

   Steering = TRUE starts the behavior after a steer: rt post to the running
   seat returned seq 3, and the waiter runs rt wait --rooms <room>:3.

   Not modeled: history pages (200 messages each). The after_seq 0 paging case
   is TestWaitAfterZeroFindsReplyBehindAFullPage. *)
EXTENDS Naturals, Sequences

CONSTANTS
    Budget,      \* max_chars
    ReplyLen,    \* characters of a seat reply
    NoticeLen,   \* characters of a system notice
    MaxTurns,    \* turns the room starts in one behavior
    MaxPosts,    \* extra user posts after the first one
    MaxSteers,   \* posts steered into a running turn
    MaxBare,     \* turns queued without a message
    MaxDrops,    \* queued turns dropped without an answer
    MaxWork,     \* work steps after a finished reply
    Steering,    \* see above
    PublishWork, \* see above
    Done         \* "window", "replied", "idle" or "current"

ASSUME Done \in {"window", "replied", "idle", "current"} /\ Steering \in BOOLEAN /\ PublishWork \in BOOLEAN

After == IF Steering THEN 3 ELSE 1   \* the seq rt post returned

VARIABLES
    msgs, queued, running, work, listed,      \* room
    turns, posts, steers, bare, drops, works, \* bounds
    signal,                                   \* pending SSE signal (one slot)
    phase, seen, checked                      \* waiter
room == <<msgs, queued, running, work, listed>>
bounds == <<turns, posts, steers, bare, drops, works>>
waiter == <<phase, seen, checked>>
vars == <<room, bounds, signal, waiter>>

Msg(k, s, n, l) == [kind |-> k, status |-> s, len |-> n, live |-> l]

\* The contract's busy room, and what the server reports as active.
Busy == queued \/ running \/ work
Reported == IF Done = "current" THEN Busy ELSE queued \/ running

Summary == <<Reported, Len(msgs)>>

\* A room step ends by publishing its summary; the feed gets a signal if it moved.
Publish(new) ==
    /\ listed' = new
    /\ signal' = (signal \/ new # listed)

\* Seqs the max_chars budget keeps, newest first. The message that crosses the
\* budget keeps its end; older ones are left out (api.go readSelected).
RECURSIVE Kept(_, _)
Kept(i, rem) ==
    IF i <= After \/ rem = 0 THEN {}
    ELSE {i} \cup Kept(i - 1, IF msgs[i].len >= rem THEN 0 ELSE rem - msgs[i].len)

IsReply(i) == msgs[i].kind = "agent"
DoneReply(i) == IsReply(i) /\ msgs[i].status = "done"

\* The reply of the turn that was running when the steer at j arrived.
TurnOf(j) == CHOOSE i \in 1..(j - 1) : IsReply(i) /\ \A k \in (i + 1)..(j - 1) : ~IsReply(k)

RepliedAbove == \E i \in (After + 1)..Len(msgs) : DoneReply(i)

Answered ==
    \/ RepliedAbove
    \/ \E j \in After..Len(msgs) : msgs[j].live /\ DoneReply(TurnOf(j))

\* The waiter's finished test on the snapshot MsgRead reads.
Finished ==
    CASE Done = "window"  -> \E i \in Kept(Len(msgs), Budget) : DoneReply(i)
      [] Done = "replied" -> RepliedAbove
      [] Done = "idle"    -> RepliedAbove /\ ~Reported
      [] Done = "current" -> Answered /\ ~Reported

Init ==
    /\ IF Steering
       THEN /\ msgs = <<Msg("user", "done", 1, FALSE), Msg("agent", "streaming", ReplyLen, FALSE), Msg("user", "done", 1, TRUE)>>
            /\ queued = FALSE /\ running = TRUE /\ turns = 1 /\ steers = 1
       ELSE /\ msgs = <<Msg("user", "done", 1, FALSE)>>
            /\ queued = TRUE /\ running = FALSE /\ turns = 0 /\ steers = 0
    /\ work = FALSE
    /\ listed = Summary
    /\ posts = 0 /\ bare = 0 /\ drops = 0 /\ works = 0
    /\ signal = FALSE     \* the waiter subscribes before its first read
    /\ phase = "list"
    /\ seen = 0
    /\ checked = 0        \* no idle read yet (a room always has messages)

-------------------------------------------------------------------------------
\* Room

Start ==
    /\ queued /\ ~running /\ turns < MaxTurns
    /\ queued' = FALSE
    /\ running' = TRUE
    /\ turns' = turns + 1
    /\ msgs' = Append(msgs, Msg("agent", "streaming", ReplyLen, FALSE))
    /\ UNCHANGED <<work, posts, steers, bare, drops, works, waiter>>
    /\ Publish(Summary')

Streaming == CHOOSE i \in 1..Len(msgs) : IsReply(i) /\ msgs[i].status = "streaming"

\* took: the turn took its steers. Otherwise they lose live and a turn is queued.
\* step: the reply ends a part and starts its submit check.
Finish ==
    /\ running
    /\ running' = FALSE
    /\ \E took, notice, step \in BOOLEAN :
        LET r == Streaming
            done == [msgs EXCEPT ![r].status = "done"]
            left == {j \in (r + 1)..Len(msgs) : msgs[j].live}
            kept == IF took THEN done
                    ELSE [j \in 1..Len(done) |-> IF j \in left THEN [done[j] EXCEPT !.live = FALSE] ELSE done[j]]
        IN  /\ (step => works < MaxWork)
            /\ msgs' = IF notice THEN Append(kept, Msg("system", "done", NoticeLen, FALSE)) ELSE kept
            /\ queued' = (queued \/ (~took /\ left # {}))
            /\ work' = step
            /\ works' = IF step THEN works + 1 ELSE works
            \* The reply's event comes before the step; without publishList the
            \* step itself publishes nothing.
            /\ Publish(IF PublishWork THEN Summary' ELSE <<queued' \/ running', Len(msgs')>>)
    /\ UNCHANGED <<turns, posts, steers, bare, drops, waiter>>

\* The work step ends: it queues the review turn, or it stops (blocked part).
WorkEnd ==
    /\ work
    /\ work' = FALSE
    /\ \E review \in BOOLEAN : queued' = (queued \/ review)
    /\ UNCHANGED <<msgs, running, bounds, waiter>>
    /\ Publish(Summary')

\* Another user message to an idle seat, e.g. from a second orchestrator.
Post ==
    /\ ~running /\ posts < MaxPosts
    /\ posts' = posts + 1
    /\ msgs' = Append(msgs, Msg("user", "done", 1, FALSE))
    /\ queued' = TRUE
    /\ UNCHANGED <<running, work, turns, steers, bare, drops, works, waiter>>
    /\ Publish(Summary')

\* A post to the running seat joins its turn and queues nothing.
Steer ==
    /\ running /\ steers < MaxSteers
    /\ steers' = steers + 1
    /\ msgs' = Append(msgs, Msg("user", "done", 1, TRUE))
    /\ UNCHANGED <<queued, running, work, turns, posts, bare, drops, works, waiter>>
    /\ Publish(Summary')

\* A turn queued without a message (discussion start, replayed turn).
QueueBare ==
    /\ ~queued /\ bare < MaxBare
    /\ bare' = bare + 1
    /\ queued' = TRUE
    /\ UNCHANGED <<msgs, running, work, turns, posts, steers, drops, works, waiter>>
    /\ Publish(Summary')

\* A queued turn leaves without an answer (CancelTurn, CancelRequest, holdTurn).
Drop ==
    /\ queued /\ drops < MaxDrops
    /\ drops' = drops + 1
    /\ queued' = FALSE
    /\ UNCHANGED <<msgs, running, work, turns, posts, steers, bare, works, waiter>>
    /\ Publish(Summary')

Room == Start \/ Finish \/ WorkEnd \/ Post \/ Steer \/ QueueBare \/ Drop

-------------------------------------------------------------------------------
\* Waiter

ListRead ==
    /\ phase = "list"
    /\ IF ~Reported /\ checked # Len(msgs)
       THEN /\ phase' = "read"
            /\ seen' = Len(msgs)
            /\ checked' = IF Done \in {"idle", "current"} THEN checked ELSE Len(msgs)
       ELSE phase' = "sleep" /\ UNCHANGED <<seen, checked>>
    /\ UNCHANGED <<room, bounds, signal>>

\* A separate request: the room may have moved since ListRead.
MsgRead ==
    /\ phase = "read"
    /\ phase' = IF Finished THEN "ready" ELSE "sleep"
    /\ checked' = IF Done \in {"idle", "current"} /\ ~Reported THEN seen ELSE checked
    /\ UNCHANGED <<room, bounds, signal, seen>>

Wake ==
    /\ phase = "sleep" /\ signal
    /\ signal' = FALSE
    /\ phase' = "list"
    /\ UNCHANGED <<room, bounds, seen, checked>>

Waiter == ListRead \/ MsgRead \/ Wake

\* Nothing left to do: the room stopped and the waiter returned or sleeps.
Halt ==
    /\ ~ENABLED (Room \/ Waiter)
    /\ UNCHANGED vars

Next == Room \/ Waiter \/ Halt

Spec == Init /\ [][Next]_vars /\ WF_vars(Waiter)

-------------------------------------------------------------------------------
\* Properties: the contract is "no running or queued turn or work step, and a
\* seat answered the message at after_seq or a later one".

TypeOK ==
    /\ phase \in {"list", "read", "sleep", "ready"}
    /\ seen \in 0..Len(msgs) /\ checked \in 0..Len(msgs)
    /\ queued \in BOOLEAN /\ running \in BOOLEAN /\ work \in BOOLEAN /\ signal \in BOOLEAN
    /\ listed \in BOOLEAN \X (0..Len(msgs))

\* ready names a room whose seat answered.
ReadyHasAnswer == phase = "ready" => Answered

\* A waiter asleep with no signal pending never sleeps through a finished room.
SleepsOnlyWhenUnfinished == (phase = "sleep" /\ ~signal) => ~(~Busy /\ Answered)

\* The read that returns ready saw no running or queued turn or work step.
\* MsgRead leaves the room unchanged, so Busy here is what that read observed.
ReadyOnlyWhenIdle == [][phase # "ready" /\ phase' = "ready" => ~Busy]_vars

\* A room that stays idle with an answer wakes the waiter.
WakesWhenFinished == <>[](~Busy /\ Answered) => <>(phase = "ready")

=============================================================================
