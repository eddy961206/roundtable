---------------------------- MODULE TurnDelivery ----------------------------
\* covers: internal/room/hub.go internal/room/steering.go internal/agent/codex_appserver.go internal/agent/claude.go
\* reach: \E m \in DOMAIN msgs : worked[m] = 1 /\ msgs[m].kind = "reply"
\* reach: \E m \in DOMAIN msgs : m \in failed /\ worked[m] = 1
\* reach: \E m \in DOMAIN msgs : receipt[m] = "accepted"
\* reach: \E m \in DOMAIN msgs : receipt[m] = "queued" /\ worked[m] = 1
(* Delivering room messages to one seat's session (internal/room/hub.go).

   The seat keeps a cursor: LastSeen and Unseen, the holes left by replies
   that were still streaming when a turn read past them. A turn has a cap
   (Turn.Cap): deltaForContext reads the messages above LastSeen and the holes
   up to the cap, skips a streaming reply as a new hole, and returns the new
   cursor. buildTurn drops a turn that has nothing to read and moves the cursor.
   runTurn moves the cursor when the turn was delivered: done, or an error
   after output (a458c08: the seat already did the work). A stop or an error
   before output leaves the messages unseen for the next turn. A failed turn
   is not retried; the person sees its error. Stop (dropping the whole
   queue) is a person's explicit discard and is left out.

   Turns come from a person's post and from another seat's reply that mentions
   this seat (chainMentions, cap = that reply). A targeted post goes ahead of
   ordinary turns but behind earlier targeted ones (enqueueFront); the rest
   go to the back (enqueue).
   Every step is one h.mu hold; the CLI run is the gap between Start and End.

   Flags turn off one rule each:
     DeliverOnOutput  a458c08: an error after output delivered nothing, so
                      the next turn read the same messages and redid the work.
     KeepHoles        a reply still streaming stays a hole; without it the
                      cursor passes it and the mention is never read.
     UseReceipts      before message receipts: Live was set on enqueue and
                      even accepted input was replayed when the turn failed.

   A steer is posted, written, accepted/rejected and acknowledged in separate
   actions. ProviderEnd precedes End (the room lock); a post can arrive in
   that gap. A missing acknowledgement is resolved from durable history at
   End. An unreadable history becomes a visible failure, never a replay.
   The provider's acceptance counts delivery, not model understanding.
   Crash recovery uses the same receipt settlement; OS process lifetime,
   storage failures, explicit user stop and page limits are outside this model. *)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS MaxMsgs, MaxFail, DeliverOnOutput, KeepHoles, UseReceipts

ASSUME DeliverOnOutput \in BOOLEAN /\ KeepHoles \in BOOLEAN /\ UseReceipts \in BOOLEAN

VARIABLES
    msgs,     \* room messages: [kind |-> "user" | "reply", status, calls] (calls: it makes this seat a turn)
    last,     \* Participant.LastSeen
    holes,    \* Participant.Unseen
    queue,    \* queued turns, front first: [cap, prio] (prio: enqueueFront)
    run,      \* the running turn: [cap, read, seen, holes]; None (cap 0) when idle
    worked,   \* turns that did the work for each message (done or error after output)
    failed,   \* messages a visibly failed turn read
    fails,
    receipt, live, accepted, rejected
vars == <<msgs, last, holes, queue, run, worked, failed, fails, receipt, live, accepted, rejected>>

None == [cap |-> 0, read |-> {}, seen |-> 0, holes |-> {}, inputs |-> {}, phase |-> "idle", outcome |-> ""]
Seqs == 1..Len(msgs)

\* deltaForContext for cap c: what the turn reads, the new LastSeen, the new holes.
Read(c) == {m \in 1..c : (m > last \/ m \in holes) /\ msgs[m].status = "done" /\ m \notin live /\ receipt[m] # "failed"}
Seen(c) == IF c > last THEN c ELSE last
Holes(c) == {m \in holes : m > c} \cup
            (IF KeepHoles THEN {m \in 1..c : (m > last \/ m \in holes) /\ msgs[m].status = "streaming"} ELSE {})

Init ==
    /\ msgs = <<>> /\ last = 0 /\ holes = {} /\ queue = <<>> /\ run = None
    /\ worked = <<>> /\ failed = {} /\ fails = 0
    /\ receipt = <<>> /\ live = {} /\ accepted = {} /\ rejected = {}

AddMsg(m) ==
    /\ Len(msgs) < MaxMsgs
    /\ msgs' = Append(msgs, m)
    /\ worked' = Append(worked, 0)
    /\ receipt' = Append(receipt, "ordinary")

-------------------------------------------------------------------------------
\* A person posts. targeted: to this seat (queue front), else to everyone (back).
Post ==
    /\ \E targeted \in BOOLEAN :
        /\ AddMsg([kind |-> "user", status |-> "done", calls |-> TRUE])
        /\ LET t == [cap |-> Len(msgs) + 1, prio |-> targeted]
               at == IF ~targeted THEN Len(queue)
                     ELSE LET ps == {i \in 1..Len(queue) : queue[i].prio} IN IF ps = {} THEN 0 ELSE CHOOSE i \in ps : \A j \in ps : j <= i
           IN queue' = SubSeq(queue, 1, at) \o <<t>> \o SubSeq(queue, at + 1, Len(queue))
    /\ UNCHANGED <<last, holes, run, failed, fails, live, accepted, rejected>>

\* Another seat starts a reply (prepareTurn appends it streaming).
OtherStart ==
    /\ \E calls \in BOOLEAN : AddMsg([kind |-> "reply", status |-> "streaming", calls |-> calls])
    /\ UNCHANGED <<last, holes, queue, run, failed, fails, live, accepted, rejected>>

\* That reply ends; a mention of this seat queues a turn capped at the reply.
OtherEnd ==
    /\ \E m \in Seqs :
        /\ msgs[m].status = "streaming"
        /\ msgs' = [msgs EXCEPT ![m].status = "done"]
        /\ queue' = IF msgs[m].calls THEN Append(queue, [cap |-> m, prio |-> FALSE]) ELSE queue
    /\ UNCHANGED <<last, holes, run, worked, failed, fails, receipt, live, accepted, rejected>>

\* pump takes the front turn: buildTurn drops it or prepareTurn starts it.
Start ==
    /\ run = None /\ queue # <<>>
    /\ LET c == Head(queue).cap IN
       /\ queue' = Tail(queue)
       /\ IF Read(c) = {}
          THEN /\ last' = Seen(c) /\ holes' = Holes(c) /\ run' = None
          ELSE /\ run' = [cap |-> c, read |-> Read(c), seen |-> Seen(c), holes |-> Holes(c), inputs |-> {}, phase |-> "running", outcome |-> ""]
               /\ UNCHANGED <<last, holes>>
    /\ UNCHANGED <<msgs, worked, failed, fails, receipt, live, accepted, rejected>>

\* acceptUser can still see a running room turn after the provider has ended.
Steer ==
    /\ run # None /\ Len(msgs) < MaxMsgs
    /\ LET m == Len(msgs) + 1 IN
       /\ msgs' = Append(msgs, [kind |-> "user", status |-> "done", calls |-> TRUE])
       /\ receipt' = Append(receipt, "pending")
       /\ worked' = Append(worked, 0)
       /\ run' = [run EXCEPT !.inputs = @ \cup {m}]
       /\ live' = IF UseReceipts THEN live ELSE live \cup {m}
    /\ UNCHANGED <<last, holes, queue, failed, fails, accepted, rejected>>

Write ==
    /\ \E m \in run.inputs :
       /\ receipt[m] = "pending"
       /\ receipt' = [receipt EXCEPT ![m] = "sending"]
    /\ UNCHANGED <<msgs, last, holes, queue, run, worked, failed, fails, live, accepted, rejected>>

ProviderAccept ==
    /\ run.phase = "running"
    /\ \E m \in run.inputs :
       /\ receipt[m] = "sending" /\ m \notin accepted /\ m \notin rejected
       /\ accepted' = accepted \cup {m}
       /\ worked' = [worked EXCEPT ![m] = @ + 1]
    /\ UNCHANGED <<msgs, last, holes, queue, run, failed, fails, receipt, live, rejected>>

ProviderReject ==
    /\ \E m \in run.inputs :
       /\ receipt[m] = "sending" /\ m \notin accepted /\ m \notin rejected
       /\ rejected' = rejected \cup {m}
    /\ UNCHANGED <<msgs, last, holes, queue, run, worked, failed, fails, receipt, live, accepted>>

Ack ==
    /\ \E m \in run.inputs :
       /\ receipt[m] = "sending" /\ m \in (accepted \cup rejected)
       /\ receipt' = [receipt EXCEPT ![m] = IF m \in accepted THEN "accepted" ELSE "queued"]
       /\ live' = IF UseReceipts THEN (IF m \in accepted THEN live \cup {m} ELSE live \ {m}) ELSE live
    /\ UNCHANGED <<msgs, last, holes, queue, run, worked, failed, fails, accepted, rejected>>

ProviderEnd ==
    /\ run.phase = "running"
    /\ \E outcome \in {"done", "output", "error", "cancelled"} :
       /\ (outcome # "done") => fails < MaxFail
       /\ run' = [run EXCEPT !.phase = "ended", !.outcome = outcome]
    /\ UNCHANGED <<msgs, last, holes, queue, worked, failed, fails, receipt, live, accepted, rejected>>

\* runTurn ends. "done" and "output" (an error after output) did the work.
End ==
    /\ run.phase = "ended"
    /\ LET outcome == run.outcome IN
        /\ (outcome # "done") => fails < MaxFail
        /\ fails' = IF outcome = "done" THEN fails ELSE fails + 1
        /\ worked' = [m \in DOMAIN worked |-> IF m \in run.read /\ outcome \in {"done", "output"} THEN worked[m] + 1 ELSE worked[m]]
        /\ IF outcome = "done" \/ (outcome = "output" /\ DeliverOnOutput)
           THEN last' = run.seen /\ holes' = run.holes
           ELSE UNCHANGED <<last, holes>>
    /\ \E historyReadable \in BOOLEAN :
       LET known == {m \in run.inputs : receipt[m] = "accepted" \/ (receipt[m] = "sending" /\ historyReadable /\ m \in accepted)}
           unknown == {m \in run.inputs : receipt[m] = "sending" /\ ~historyReadable}
           kept == IF UseReceipts THEN known ELSE (IF run.outcome = "done" THEN run.inputs \cap accepted ELSE {})
           blocked == IF UseReceipts THEN unknown ELSE {}
           retry == run.inputs \ (kept \cup blocked)
           caps == IF retry = {} THEN <<>> ELSE <<[cap |-> CHOOSE m \in retry : \A n \in retry : n <= m, prio |-> TRUE]>>
       IN /\ live' = (live \ run.inputs) \cup kept
          /\ receipt' = [m \in DOMAIN receipt |-> IF m \in kept THEN "accepted" ELSE IF m \in blocked THEN "failed" ELSE IF m \in retry THEN "queued" ELSE receipt[m]]
          /\ queue' = caps \o queue
          /\ failed' = failed \cup blocked \cup (IF run.outcome = "done" THEN {} ELSE run.read)
    /\ run' = None
    /\ UNCHANGED <<msgs, accepted, rejected>>

Step == Post \/ OtherStart \/ OtherEnd \/ Start \/ Steer \/ Write \/ ProviderAccept \/ ProviderReject \/ Ack \/ ProviderEnd \/ End

Halt == ~ENABLED Step /\ UNCHANGED vars

Next == Step \/ Halt

Spec == Init /\ [][Next]_vars /\ WF_vars(Start) /\ WF_vars(ProviderEnd) /\ WF_vars(End) /\ WF_vars(OtherEnd)

-------------------------------------------------------------------------------
\* Properties

TypeOK == run.cap \in 0..Len(msgs)
ReceiptBeforeLive == live \subseteq accepted

\* The seat does the work for a message once: no second turn reads a message
\* a turn already answered (done or error after output).
ExactlyOnceWork == \A m \in DOMAIN worked : worked[m] <= 1

\* Once the room is quiet, every message that called the seat was answered, or
\* a turn that read it failed where the person sees the error.
Quiet == run = None /\ queue = <<>> /\ \A m \in Seqs : msgs[m].status = "done"
NothingSilentlyLost == Quiet => \A m \in Seqs : msgs[m].calls => (worked[m] >= 1 \/ m \in failed)

\* The room always gets quiet: no turn waits forever.
EventuallyQuiet == <>[]Quiet

=============================================================================
