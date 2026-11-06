------------------------------- MODULE RtWait -------------------------------
\* covers: internal/rtmcp/wait.go internal/rtmcp/api.go internal/room/hub.go internal/room/history.go internal/room/lifecycle.go internal/room/work.go internal/room/work_cleanup.go internal/room/user_requests.go internal/room/requests.go internal/rtmcp/observe.go internal/room/steering.go
\* reach: phase = "ready"
\* reach: (MaxRequests = 0 /\ phase = "ready") \/ phase = "requested"
\* reach: (MaxResumes = 0 \/ resumes = 1) /\ phase = "sleep"
\* reach: (MaxSteers = 0 /\ phase = "ready") \/ \E j \in 1..Len(msgs) : msgs[j].receipt = "accepted"
\* reach: (MaxSteers = 0 /\ phase = "ready") \/ \E j \in 1..Len(msgs) : msgs[j].receipt = "failed"
\* reach: ((MaxRequests = 0 \/ MaxSteers = 0) /\ phase = "ready") \/ \E j \in 1..Len(msgs) : msgs[j].answersRequest /\ msgs[j].receipt = "failed" /\ pending = {}
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

   User requests: a completed reply may append a user_request notice while
   another turn is queued. It remains pending until Resolve. Receipt is the
   third component of the caller's rooms argument, initially 0, returned in
   waiting; Resume models a new call with that value. InitialRequest models a
   notice that existed before the call (including a restored legacy notice).
   Notify = "notice" is the old code (only a notice); "level" returns for
   every pending request without checking its receipt; "current" uses it.
   Request numbers abstract monotonically increasing notice message seqs.
   "requested" denotes user_requested even if the same return also has ready
   rooms; "ready" here denotes a return caused only by a completed room.
   ListRead and RequestRead are separate HTTP reads; a request can be resolved
   between them. Receipt changes only on a returned snapshot, not on subscribe.

   UseReceipts separates enqueue, write, provider acceptance and acknowledgement.
   The receipt stores the exact reply/turn. Finish settles receipts before
   publishing idle; a write by itself is not an answer. UseReceipts = FALSE
   models the former Live + successful-write count, which could claim an
   unreceived input was answered. Missing acknowledgement is resolved from
   history; unreadable history is a failed input, not a rejection or a replay.
   ExposeFailures = FALSE models a wait returning only the older turn's answer
   while hiding that failed input at after_seq (delivery_test.go).
   Resolve models both acknowledgement and answering through the same post/
   steer path. A pending request's receipt cursor never means input acceptance.

   Not modeled: disk failures, process restart, Ask counts, multiple rooms and
   history pages (200 messages each). These have Go regression tests. The after_seq 0 paging case
   is TestWaitAfterZeroFindsReplyBehindAFullPage. *)
EXTENDS Naturals, Sequences, FiniteSets

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
    Done,        \* "window", "replied", "idle" or "current"
    MaxRequests, MaxResumes, InitialRequest, Notify, UseReceipts, ExposeFailures

ASSUME Done \in {"window", "replied", "idle", "current"} /\ Steering \in BOOLEAN /\ PublishWork \in BOOLEAN /\ UseReceipts \in BOOLEAN
ASSUME Notify \in {"notice", "level", "current"} /\ InitialRequest \in BOOLEAN
ASSUME ExposeFailures \in BOOLEAN

After == IF Steering THEN 3 ELSE 1   \* the seq rt post returned

VARIABLES
    msgs, queued, running, work, listed,      \* room
    turns, posts, steers, bare, drops, works, \* bounds
    signal,                                   \* pending SSE signal (one slot)
    phase, seen, checked, receipt, resumes, shownFailures, \* waiter
    pending, requests                         \* user requests, issued count
room == <<msgs, queued, running, work, listed, pending, requests>>
bounds == <<turns, posts, steers, bare, drops, works>>
waiter == <<phase, seen, checked, receipt, resumes, shownFailures>>
vars == <<room, bounds, signal, waiter>>

Msg(k, s, n, l) == [kind |-> k, status |-> s, len |-> n, live |-> l, receipt |-> "ordinary", accepted |-> FALSE, targetTurn |-> 0, answersRequest |-> FALSE]
InputTo(r) == [Msg("user", "done", 1, ~UseReceipts) EXCEPT !.receipt = "pending", !.targetTurn = r]

\* The contract's busy room, and what the server reports as active.
Busy == queued \/ running \/ work
Reported == IF Done = "current" THEN Busy ELSE queued \/ running

LatestPending == IF pending = {} THEN 0 ELSE CHOOSE n \in pending : \A i \in pending : n >= i
Summary == <<Reported, Len(msgs), Cardinality(pending), LatestPending>>
UnseenRequest == \E i \in pending : i > receipt
NotifyRequest == CASE Notify = "notice" -> FALSE
                   [] Notify = "level" -> pending # {}
                   [] Notify = "current" -> UnseenRequest

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
    \/ \E j \in After..Len(msgs) : msgs[j].accepted /\ DoneReply(msgs[j].targetTurn)

Failures == {j \in After..Len(msgs) : msgs[j].receipt = "failed" /\ DoneReply(msgs[j].targetTurn)}
Settled == Answered \/ Failures # {}

ObservedAnswered ==
    \/ RepliedAbove
    \/ \E j \in After..Len(msgs) :
       IF UseReceipts THEN msgs[j].receipt \in {"accepted", "failed"} /\ DoneReply(msgs[j].targetTurn)
       ELSE msgs[j].live /\ DoneReply(TurnOf(j))

\* The waiter's finished test on the snapshot MsgRead reads.
Finished ==
    CASE Done = "window"  -> \E i \in Kept(Len(msgs), Budget) : DoneReply(i)
      [] Done = "replied" -> RepliedAbove
      [] Done = "idle"    -> RepliedAbove /\ ~Reported
      [] Done = "current" -> ObservedAnswered /\ ~Reported

Init ==
    /\ IF Steering
       THEN /\ msgs = <<Msg("user", "done", 1, FALSE), Msg("agent", "streaming", ReplyLen, FALSE), InputTo(2)>>
            /\ queued = FALSE /\ running = TRUE /\ turns = 1 /\ steers = 1
       ELSE /\ msgs = <<Msg("user", "done", 1, FALSE)>>
            /\ queued = TRUE /\ running = FALSE /\ turns = 0 /\ steers = 0
    /\ work = FALSE
    /\ requests = IF InitialRequest THEN 1 ELSE 0
    /\ pending = IF InitialRequest THEN {1} ELSE {}
    /\ listed = Summary
    /\ posts = 0 /\ bare = 0 /\ drops = 0 /\ works = 0
    /\ signal = FALSE     \* the waiter subscribes before its first read
    /\ phase = "list"
    /\ seen = 0
    /\ checked = 0        \* no idle read yet (a room always has messages)
    /\ receipt = 0 /\ resumes = 0 /\ shownFailures = {}

-------------------------------------------------------------------------------
\* Room

Start ==
    /\ queued /\ ~running /\ turns < MaxTurns
    /\ queued' = FALSE
    /\ running' = TRUE
    /\ turns' = turns + 1
    /\ msgs' = Append(msgs, Msg("agent", "streaming", ReplyLen, FALSE))
    /\ UNCHANGED <<work, posts, steers, bare, drops, works, waiter, pending, requests>>
    /\ Publish(Summary')

Streaming == CHOOSE i \in 1..Len(msgs) : IsReply(i) /\ msgs[i].status = "streaming"

\* took: the turn took its steers. Otherwise they lose live and a turn is queued.
\* step: the reply ends a part and starts its submit check.
Finish ==
    /\ running
    /\ running' = FALSE
    /\ \E took, notice, step, userRequest, historyReadable \in BOOLEAN :
        LET r == Streaming
            done == [msgs EXCEPT ![r].status = "done"]
            left == {j \in (r + 1)..Len(msgs) : msgs[j].targetTurn = r}
            confirmed == {j \in left : msgs[j].receipt = "accepted" \/ (msgs[j].receipt = "sending" /\ historyReadable /\ msgs[j].accepted)}
            unknown == {j \in left : msgs[j].receipt = "sending" /\ ~historyReadable}
            delivered == IF UseReceipts THEN confirmed ELSE (IF took THEN left ELSE {})
            failed == IF UseReceipts THEN unknown ELSE {}
            kept == [j \in 1..Len(done) |-> IF j \in left THEN [done[j] EXCEPT !.live = (j \in delivered), !.receipt = IF j \in delivered THEN "accepted" ELSE IF j \in failed THEN "failed" ELSE "queued"] ELSE done[j]]
        IN  /\ (step => works < MaxWork)
            /\ (userRequest => requests < MaxRequests)
            /\ msgs' = IF notice \/ userRequest THEN Append(kept, Msg("system", "done", NoticeLen, FALSE)) ELSE kept
            /\ requests' = IF userRequest THEN requests + 1 ELSE requests
            /\ pending' = IF userRequest THEN pending \cup {requests + 1} ELSE pending
            /\ queued' = (queued \/ (left \ (delivered \cup failed) # {}))
            /\ work' = step
            /\ works' = IF step THEN works + 1 ELSE works
            \* The reply's event comes before the step; without publishList the
            \* step itself publishes nothing.
            /\ Publish(IF PublishWork THEN Summary' ELSE <<queued' \/ running', Len(msgs'), Cardinality(pending'), LatestPending'>>)
    /\ UNCHANGED <<turns, posts, steers, bare, drops, waiter>>

\* The work step ends: it queues the review turn, or it stops (blocked part).
WorkEnd ==
    /\ work
    /\ work' = FALSE
    /\ \E review \in BOOLEAN : queued' = (queued \/ review)
    /\ UNCHANGED <<msgs, running, bounds, waiter, pending, requests>>
    /\ Publish(Summary')

\* Another user message to an idle seat, e.g. from a second orchestrator.
Post ==
    /\ ~running /\ posts < MaxPosts
    /\ posts' = posts + 1
    /\ msgs' = Append(msgs, Msg("user", "done", 1, FALSE))
    /\ queued' = TRUE
    /\ UNCHANGED <<running, work, turns, steers, bare, drops, works, waiter, pending, requests>>
    /\ Publish(Summary')

\* A post to the running seat joins its turn and queues nothing.
Steer ==
    /\ running /\ steers < MaxSteers
    /\ steers' = steers + 1
    /\ msgs' = Append(msgs, InputTo(Streaming))
    /\ UNCHANGED <<queued, running, work, turns, posts, bare, drops, works, waiter, pending, requests>>
    /\ Publish(Summary')

WriteInput ==
    /\ running
    /\ \E j \in 1..Len(msgs) :
       /\ msgs[j].targetTurn = Streaming /\ msgs[j].receipt = "pending"
       /\ msgs' = [msgs EXCEPT ![j].receipt = "sending"]
    /\ UNCHANGED <<queued, running, work, bounds, waiter, pending, requests>>
    /\ Publish(Summary')

AcceptInput ==
    /\ running
    /\ \E j \in 1..Len(msgs) :
       /\ msgs[j].targetTurn = Streaming /\ msgs[j].receipt = "sending" /\ ~msgs[j].accepted
       /\ msgs' = [msgs EXCEPT ![j].accepted = TRUE]
    /\ UNCHANGED <<queued, running, work, bounds, waiter, pending, requests>>
    /\ Publish(Summary')

AckInput ==
    /\ running
    /\ \E j \in 1..Len(msgs) :
       /\ msgs[j].targetTurn = Streaming /\ msgs[j].receipt = "sending" /\ msgs[j].accepted
       /\ msgs' = [msgs EXCEPT ![j].receipt = "accepted", ![j].live = TRUE]
    /\ UNCHANGED <<queued, running, work, bounds, waiter, pending, requests>>
    /\ Publish(Summary')

\* A turn queued without a message (discussion start, replayed turn).
QueueBare ==
    /\ ~queued /\ bare < MaxBare
    /\ bare' = bare + 1
    /\ queued' = TRUE
    /\ UNCHANGED <<msgs, running, work, turns, posts, steers, drops, works, waiter, pending, requests>>
    /\ Publish(Summary')

\* A queued turn leaves without an answer (CancelTurn, CancelRequest, holdTurn).
Drop ==
    /\ queued /\ drops < MaxDrops
    /\ drops' = drops + 1
    /\ queued' = FALSE
    /\ UNCHANGED <<msgs, running, work, turns, posts, steers, bare, works, waiter, pending, requests>>
    /\ Publish(Summary')

\* Both choices remove pending only once. An answer creates ordinary input
\* or a pending steer. Its later delivery failure does not reopen the request.
Resolve ==
    /\ pending # {}
    /\ \E i \in pending : pending' = pending \ {i}
    /\ \E answer \in BOOLEAN :
       /\ (answer /\ running => steers < MaxSteers)
       /\ (answer /\ ~running => posts < MaxPosts)
       /\ LET input == IF running THEN InputTo(Streaming) ELSE Msg("user", "done", 1, FALSE)
          IN msgs' = Append(msgs, IF answer THEN [input EXCEPT !.answersRequest = TRUE] ELSE Msg("system", "done", NoticeLen, FALSE))
       /\ queued' = (queued \/ (answer /\ ~running))
       /\ steers' = IF answer /\ running THEN steers + 1 ELSE steers
       /\ posts' = IF answer /\ ~running THEN posts + 1 ELSE posts
    /\ UNCHANGED <<running, work, requests, turns, bare, drops, works, waiter>>
    /\ Publish(Summary')

Room == Start \/ Finish \/ WorkEnd \/ Post \/ Steer \/ WriteInput \/ AcceptInput \/ AckInput \/ QueueBare \/ Drop \/ Resolve

-------------------------------------------------------------------------------
\* Waiter

ListRead ==
    /\ phase = "list"
    /\ IF ~Reported /\ checked # Len(msgs)
       THEN /\ phase' = "read"
            /\ seen' = Len(msgs)
            /\ checked' = IF Done \in {"idle", "current"} THEN checked ELSE Len(msgs)
       ELSE /\ phase' = IF Notify # "notice" /\ pending # {} THEN "request_read" ELSE "sleep"
            /\ UNCHANGED <<seen, checked>>
    /\ UNCHANGED <<room, bounds, signal, receipt, resumes, shownFailures>>

\* A separate request: the room may have moved since ListRead.
MsgRead ==
    /\ phase = "read"
    /\ phase' = IF NotifyRequest THEN "requested" ELSE IF Finished THEN "ready" ELSE "sleep"
    /\ receipt' = IF NotifyRequest THEN LatestPending ELSE receipt
    /\ shownFailures' = IF ExposeFailures THEN Failures ELSE {}
    /\ checked' = IF Done \in {"idle", "current"} /\ ~Reported THEN seen ELSE checked
    /\ UNCHANGED <<room, bounds, signal, seen, resumes>>

RequestRead ==
    /\ phase = "request_read"
    /\ phase' = IF NotifyRequest THEN "requested" ELSE "sleep"
    /\ receipt' = IF NotifyRequest THEN LatestPending ELSE receipt
    /\ UNCHANGED <<room, bounds, signal, seen, checked, resumes, shownFailures>>

Wake ==
    /\ phase = "sleep" /\ signal
    /\ signal' = FALSE
    /\ phase' = "list"
    /\ UNCHANGED <<room, bounds, seen, checked, receipt, resumes, shownFailures>>

Resume ==
    /\ phase = "requested" /\ resumes < MaxResumes
    /\ resumes' = resumes + 1
    /\ phase' = "list" /\ checked' = 0 /\ seen' = 0 /\ signal' = FALSE
    /\ shownFailures' = {}
    /\ UNCHANGED <<room, bounds, receipt>>

Waiter == ListRead \/ MsgRead \/ RequestRead \/ Wake \/ Resume

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
    /\ phase \in {"list", "read", "request_read", "sleep", "ready", "requested"}
    /\ seen \in 0..Len(msgs) /\ checked \in 0..Len(msgs)
    /\ queued \in BOOLEAN /\ running \in BOOLEAN /\ work \in BOOLEAN /\ signal \in BOOLEAN
    /\ listed \in BOOLEAN \X (0..Len(msgs)) \X (0..MaxRequests) \X (0..MaxRequests)
    /\ pending \subseteq 1..requests /\ requests \in 0..MaxRequests
    /\ receipt \in 0..requests /\ resumes \in 0..MaxResumes
    /\ shownFailures \subseteq After..Len(msgs)

\* ready names a room whose seat answered or whose delivery visibly failed.
ReadyHasAnswer == phase = "ready" => Settled
FailureIsVisible == [][phase # "ready" /\ phase' = "ready" => Failures \subseteq shownFailures']_vars

\* A waiter asleep with no signal pending never sleeps through a finished room.
SleepsOnlyWhenUnfinished == (phase = "sleep" /\ ~signal) => ~(~Busy /\ Settled)

SleepsOnlyWhenNoUserRequest == (phase = "sleep" /\ ~signal) => ~UnseenRequest

\* A notification must contain a currently pending, not previously receipted
\* request. Resolve before RequestRead cannot create a spurious notification.
NotifiesOnlyUnseenRequest == [][phase # "requested" /\ phase' = "requested" => UnseenRequest]_vars

\* The read that returns ready saw no running or queued turn or work step.
\* MsgRead leaves the room unchanged, so Busy here is what that read observed.
ReadyOnlyWhenIdle == [][phase # "ready" /\ phase' = "ready" => ~Busy]_vars

\* A room that stays idle with an answer wakes the waiter.
WakesWhenFinished == <>[](~Busy /\ Settled) => <>(phase \in {"ready", "requested"})

\* Only a live wait owes a notification. Requests created after a completed
\* wait has returned are handled by the caller's next call, not that old one.
WakesWhenUserRequests ==
    (phase \in {"list", "read", "request_read", "sleep"} /\ UnseenRequest)
        ~> (phase = "requested" \/ ~UnseenRequest)

=============================================================================
