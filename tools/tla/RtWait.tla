------------------------------- MODULE RtWait -------------------------------
(* rt wait (internal/rtmcp/wait.go) against one room's turn state machine.

   Room (internal/room/hub.go), every step under h.mu:
     queued --Start--> running --Finish--> idle
     Start appends the seat reply as "streaming". Finish marks it "done" and,
     in the same critical section, may append a system notice (the input-token
     warning of budget.go usageNotices). A turn can also be queued without a
     message (a discussion start, a replayed turn) and leave the queue without
     an answer (CancelTurn, CancelRequest, holdTurn). A post to a running seat
     is steered into its turn and queues nothing. Every step changes the room
     summary (active or message count), so it signals the room-list SSE feed.

   Waiter: the feed holds at most one pending signal (subscribeRooms).
   readyRooms reads GET /api/rooms (ListRead), then, in a separate request,
   the room snapshot with its messages (MsgRead: rt_read with max_chars). An
   idle room whose list count equals the cached one (checked) is not read.

   Done chooses the waiter's version:
     "window"  - before 5d6506c: a done seat reply among the messages the
                 max_chars budget kept (newest first); checked is set at
                 ListRead.
     "replied" - 5d6506c: a done seat reply above after_seq, kept or not;
                 checked is set at ListRead.
     "current" - a done seat reply above after_seq and no running or queued
                 turn in the same snapshot (its queue lists both); checked is
                 set only when that snapshot was idle.

   Not modeled: history pages (200 messages each). With after_seq 0 rt_read
   stops paging once max_chars is spent, so a reply behind 200 newer non-reply
   messages is not seen; rooms without after_seq start from the room's seq. *)
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
    Done         \* "window", "replied" or "current"

ASSUME Done \in {"window", "replied", "current"}

After == 1   \* rt wait --rooms <room>:1, the first user message

VARIABLES
    msgs, queued, running,          \* room
    turns, posts, steers, bare, drops,  \* bounds
    signal,                         \* pending SSE signal (one slot)
    phase, seen, checked            \* waiter
room == <<msgs, queued, running>>
bounds == <<turns, posts, steers, bare, drops>>
waiter == <<phase, seen, checked>>
vars == <<room, bounds, signal, waiter>>

Msg(k, s, n) == [kind |-> k, status |-> s, len |-> n]

Active == queued \/ running

\* Seqs the max_chars budget keeps, newest first. The message that crosses the
\* budget keeps its end; older ones are left out (api.go readSelected).
RECURSIVE Kept(_, _)
Kept(i, rem) ==
    IF i <= After \/ rem = 0 THEN {}
    ELSE {i} \cup Kept(i - 1, IF msgs[i].len >= rem THEN 0 ELSE rem - msgs[i].len)

DoneReply(i) == msgs[i].kind = "agent" /\ msgs[i].status = "done"

Replied == \E i \in (After + 1)..Len(msgs) : DoneReply(i)

\* The waiter's finished test on the snapshot MsgRead reads.
Finished ==
    CASE Done = "window"  -> \E i \in Kept(Len(msgs), Budget) : DoneReply(i)
      [] Done = "replied" -> Replied
      [] Done = "current" -> Replied /\ ~Active

Init ==
    /\ msgs = <<Msg("user", "done", 1)>>
    /\ queued = TRUE      \* the post queued the seat's turn
    /\ running = FALSE
    /\ turns = 0 /\ posts = 0 /\ steers = 0 /\ bare = 0 /\ drops = 0
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
    /\ msgs' = Append(msgs, Msg("agent", "streaming", ReplyLen))
    /\ signal' = TRUE
    /\ UNCHANGED <<posts, steers, bare, drops, waiter>>

Finish ==
    /\ running
    /\ running' = FALSE
    /\ \E notice \in BOOLEAN :
        LET done == [msgs EXCEPT ![Len(msgs)].status = "done"]
        IN  msgs' = IF notice THEN Append(done, Msg("system", "done", NoticeLen)) ELSE done
    /\ signal' = TRUE
    /\ UNCHANGED <<queued, bounds, waiter>>

\* Another user message to an idle seat, e.g. from a second orchestrator.
Post ==
    /\ ~running /\ posts < MaxPosts
    /\ posts' = posts + 1
    /\ msgs' = Append(msgs, Msg("user", "done", 1))
    /\ queued' = TRUE
    /\ signal' = TRUE
    /\ UNCHANGED <<running, turns, steers, bare, drops, waiter>>

\* A post to the running seat joins its turn and queues nothing.
Steer ==
    /\ running /\ steers < MaxSteers
    /\ steers' = steers + 1
    /\ msgs' = Append(msgs, Msg("user", "done", 1))
    /\ signal' = TRUE
    /\ UNCHANGED <<queued, running, turns, posts, bare, drops, waiter>>

\* A turn queued without a message (discussion start, replayed turn).
QueueBare ==
    /\ ~queued /\ bare < MaxBare
    /\ bare' = bare + 1
    /\ queued' = TRUE
    /\ signal' = TRUE
    /\ UNCHANGED <<msgs, running, turns, posts, steers, drops, waiter>>

\* A queued turn leaves without an answer (CancelTurn, CancelRequest, holdTurn).
Drop ==
    /\ queued /\ drops < MaxDrops
    /\ drops' = drops + 1
    /\ queued' = FALSE
    /\ signal' = TRUE
    /\ UNCHANGED <<msgs, running, turns, posts, steers, bare, waiter>>

Room == Start \/ Finish \/ Post \/ Steer \/ QueueBare \/ Drop

-------------------------------------------------------------------------------
\* Waiter

ListRead ==
    /\ phase = "list"
    /\ IF ~Active /\ checked # Len(msgs)
       THEN /\ phase' = "read"
            /\ seen' = Len(msgs)
            /\ checked' = IF Done = "current" THEN checked ELSE Len(msgs)
       ELSE phase' = "sleep" /\ UNCHANGED <<seen, checked>>
    /\ UNCHANGED <<room, bounds, signal>>

\* A separate request: the room may have moved since ListRead.
MsgRead ==
    /\ phase = "read"
    /\ phase' = IF Finished THEN "ready" ELSE "sleep"
    /\ checked' = IF Done = "current" /\ ~Active THEN seen ELSE checked
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
\* Properties

TypeOK ==
    /\ phase \in {"list", "read", "sleep", "ready"}
    /\ seen \in 0..Len(msgs) /\ checked \in 0..Len(msgs)
    /\ queued \in BOOLEAN /\ running \in BOOLEAN /\ signal \in BOOLEAN

\* ready names a room that has a finished seat reply above after_seq.
ReadyHasReply == phase = "ready" => Replied

\* A waiter asleep with no signal pending never sleeps through a finished room.
SleepsOnlyWhenUnfinished == (phase = "sleep" /\ ~signal) => ~(~Active /\ Replied)

\* The read that returns ready saw no running or queued turn. MsgRead leaves the
\* room unchanged, so Active here is the state that read observed.
ReadyOnlyWhenIdle == [][phase # "ready" /\ phase' = "ready" => ~Active]_vars

\* A room that stays idle with a finished reply wakes the waiter.
WakesWhenFinished == <>[](~Active /\ Replied) => <>(phase = "ready")

=============================================================================
