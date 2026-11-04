---------------------------- MODULE TurnDelivery ----------------------------
\* covers: internal/room/hub.go
\* reach: \E m \in DOMAIN msgs : worked[m] = 1 /\ msgs[m].kind = "reply"
\* reach: \E m \in DOMAIN msgs : m \in failed /\ worked[m] = 1
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
                      cursor passes it and the mention is never read. *)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS MaxMsgs, MaxFail, DeliverOnOutput, KeepHoles

ASSUME DeliverOnOutput \in BOOLEAN /\ KeepHoles \in BOOLEAN

VARIABLES
    msgs,     \* room messages: [kind |-> "user" | "reply", status, calls] (calls: it makes this seat a turn)
    last,     \* Participant.LastSeen
    holes,    \* Participant.Unseen
    queue,    \* queued turns, front first: [cap, prio] (prio: enqueueFront)
    run,      \* the running turn: [cap, read, seen, holes]; None (cap 0) when idle
    worked,   \* turns that did the work for each message (done or error after output)
    failed,   \* messages a visibly failed turn read
    fails
vars == <<msgs, last, holes, queue, run, worked, failed, fails>>

None == [cap |-> 0, read |-> {}, seen |-> 0, holes |-> {}]
Seqs == 1..Len(msgs)

\* deltaForContext for cap c: what the turn reads, the new LastSeen, the new holes.
Read(c) == {m \in 1..c : (m > last \/ m \in holes) /\ msgs[m].status = "done"}
Seen(c) == IF c > last THEN c ELSE last
Holes(c) == {m \in holes : m > c} \cup
            (IF KeepHoles THEN {m \in 1..c : (m > last \/ m \in holes) /\ msgs[m].status = "streaming"} ELSE {})

Init ==
    /\ msgs = <<>> /\ last = 0 /\ holes = {} /\ queue = <<>> /\ run = None
    /\ worked = <<>> /\ failed = {} /\ fails = 0

AddMsg(m) ==
    /\ Len(msgs) < MaxMsgs
    /\ msgs' = Append(msgs, m)
    /\ worked' = Append(worked, 0)

-------------------------------------------------------------------------------
\* A person posts. targeted: to this seat (queue front), else to everyone (back).
Post ==
    /\ \E targeted \in BOOLEAN :
        /\ AddMsg([kind |-> "user", status |-> "done", calls |-> TRUE])
        /\ LET t == [cap |-> Len(msgs) + 1, prio |-> targeted]
               at == IF ~targeted THEN Len(queue)
                     ELSE LET ps == {i \in 1..Len(queue) : queue[i].prio} IN IF ps = {} THEN 0 ELSE CHOOSE i \in ps : \A j \in ps : j <= i
           IN queue' = SubSeq(queue, 1, at) \o <<t>> \o SubSeq(queue, at + 1, Len(queue))
    /\ UNCHANGED <<last, holes, run, failed, fails>>

\* Another seat starts a reply (prepareTurn appends it streaming).
OtherStart ==
    /\ \E calls \in BOOLEAN : AddMsg([kind |-> "reply", status |-> "streaming", calls |-> calls])
    /\ UNCHANGED <<last, holes, queue, run, failed, fails>>

\* That reply ends; a mention of this seat queues a turn capped at the reply.
OtherEnd ==
    /\ \E m \in Seqs :
        /\ msgs[m].status = "streaming"
        /\ msgs' = [msgs EXCEPT ![m].status = "done"]
        /\ queue' = IF msgs[m].calls THEN Append(queue, [cap |-> m, prio |-> FALSE]) ELSE queue
    /\ UNCHANGED <<last, holes, run, worked, failed, fails>>

\* pump takes the front turn: buildTurn drops it or prepareTurn starts it.
Start ==
    /\ run = None /\ queue # <<>>
    /\ LET c == Head(queue).cap IN
       /\ queue' = Tail(queue)
       /\ IF Read(c) = {}
          THEN /\ last' = Seen(c) /\ holes' = Holes(c) /\ run' = None
          ELSE /\ run' = [cap |-> c, read |-> Read(c), seen |-> Seen(c), holes |-> Holes(c)]
               /\ UNCHANGED <<last, holes>>
    /\ UNCHANGED <<msgs, worked, failed, fails>>

\* runTurn ends. "done" and "output" (an error after output) did the work.
End ==
    /\ run # None
    /\ \E outcome \in {"done", "output", "error", "cancelled"} :
        /\ (outcome # "done") => fails < MaxFail
        /\ fails' = IF outcome = "done" THEN fails ELSE fails + 1
        /\ worked' = [m \in DOMAIN worked |-> IF m \in run.read /\ outcome \in {"done", "output"} THEN worked[m] + 1 ELSE worked[m]]
        /\ failed' = IF outcome = "done" THEN failed ELSE failed \cup run.read
        /\ IF outcome = "done" \/ (outcome = "output" /\ DeliverOnOutput)
           THEN last' = run.seen /\ holes' = run.holes
           ELSE UNCHANGED <<last, holes>>
    /\ run' = None
    /\ UNCHANGED <<msgs, queue>>

Step == Post \/ OtherStart \/ OtherEnd \/ Start \/ End

Halt == ~ENABLED Step /\ UNCHANGED vars

Next == Step \/ Halt

Spec == Init /\ [][Next]_vars /\ WF_vars(Start) /\ WF_vars(End) /\ WF_vars(OtherEnd)

-------------------------------------------------------------------------------
\* Properties

TypeOK == run.cap \in 0..Len(msgs)

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
