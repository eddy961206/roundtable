----------------------------- MODULE WorkMerge -----------------------------
\* covers: internal/room/work.go internal/room/work_cleanup.go internal/room/hub.go
\* reach: \E p \in Parts : st[p] = "merged"
\* reach: \A p \in Parts : st[p] = "merged"
\* reach: restarts > 0 /\ \E p \in Parts : st[p] = "merged"
(* The merge end of a work plan (internal/room/work.go), parts that passed
   review onward, with the worktree cleanup (work_cleanup.go) and the restart
   recovery (recoverWork, resumeRecoveredWork).

   Every step below is one h.mu hold unless it says "outside h.mu".
     queuePart puts a passed part at the end of plan.Queue; advanceQueue starts
     the head: blocked{worktree} if its worktree is gone, awaiting_approval in
     confirm mode without a valid approval, else merging, and the mergePart
     goroutine runs mergeAndVerify outside h.mu. On success mergePart saves
     plan.Merging (the intent) to disk, then moves the integration branch with
     git update-ref outside h.mu, then completes (merged) or blocks
     (conflict). advanceQueue waits while a part is blocked (Halted), while a
     merge runs or its intent is on disk, and while the cleanup runs. The
     cleanup starts once no merge runs; it removes merged parts' worktrees and,
     when every part ended, the integration worktree, outside h.mu.
   A person approves, retries a blocked part (queued again) or abandons a part.
   A restart can happen at any point: goroutines vanish and recoverWork reads
   room.json and the integration branch tip.

   tip counts the commits mergePart put on the integration branch.

   Flags turn off one guard each:
     SaveIntent   D13: save plan.Merging before update-ref. Without it a
                  restart between the ref move and the merged save cannot tell
                  whether the part merged.
     GateCleanup  advanceQueue waits for the cleanup.
     GateHalted   advanceQueue waits while a part is blocked (D5). *)
EXTENDS Naturals, FiniteSets, Sequences

CONSTANTS Parts, Confirm, MaxRestart, MaxFail, SaveIntent, GateCleanup, GateHalted

ASSUME SaveIntent \in BOOLEAN /\ GateCleanup \in BOOLEAN /\ GateHalted \in BOOLEAN /\ Confirm \subseteq Parts

VARIABLES
    st,         \* Part.State
    q,          \* plan.Queue
    approved,   \* part.Approval is valid
    gone,       \* part.WorktreeRemoved
    held,       \* why a part is blocked: "verify", "queue" (conflict, worktree), "review", "restart" (from reviewing)
    mstep,      \* mergePart goroutine: "none", "verify", "move" (update-ref), "moved", "refused"
    intent,     \* plan.Merging on disk
    tip,        \* commits on the integration branch
    cleaning,   \* workBusy[busyCleanup]: the cleanup goroutine runs
    restarts, fails
vars == <<st, q, approved, gone, held, mstep, intent, tip, cleaning, restarts, fails>>

Halted == \E p \in Parts : st[p] = "blocked"
Merged == {p \in Parts : st[p] = "merged"}
Front == q[1]

\* worktreeCleanupTargets: no merge in progress, and a merged part keeps its worktree.
DueIn(s) == /\ \A p \in Parts : s[p] # "merging"
           /\ \E p \in Parts : s[p] = "merged" /\ ~gone[p]
CleanupDue == DueIn(st) /\ ~intent

\* advanceQueue as the next state of st (and mstep). Callers use it after their own change.
Advance(s, queue) ==
    IF /\ queue # <<>>
       /\ s[queue[1]] = "queued"
       /\ (GateCleanup => ~cleaning')
       /\ (GateHalted => ~\E p \in Parts : s[p] = "blocked")
       /\ ~\E p \in Parts : s[p] = "merging"
       /\ ~intent'
    THEN LET h == queue[1] IN
         IF gone[h] THEN [s EXCEPT ![h] = "blocked"]
         ELSE IF h \in Confirm /\ ~approved'[h] THEN [s EXCEPT ![h] = "awaiting_approval"]
         ELSE [s EXCEPT ![h] = "merging"]
    ELSE s

StartsMerge(s, s2) == \E p \in Parts : s[p] # "merging" /\ s2[p] = "merging"

Init ==
    /\ st = [p \in Parts |-> "reviewing"]
    /\ q = <<>> /\ approved = [p \in Parts |-> FALSE] /\ gone = [p \in Parts |-> FALSE]
    /\ held = [p \in Parts |-> "queue"]
    /\ mstep = "none" /\ intent = FALSE /\ tip = 0 /\ cleaning = FALSE
    /\ restarts = 0 /\ fails = 0

-------------------------------------------------------------------------------
\* Review passes: queuePart, then advanceQueue.
Pass(p) ==
    /\ st[p] = "reviewing"
    /\ q' = Append(q, p)
    /\ UNCHANGED <<approved, gone, held, intent, tip, cleaning, restarts, fails>>
    /\ LET s == [st EXCEPT ![p] = "queued"]
           n == Advance(s, q')
       IN st' = n /\ mstep' = IF StartsMerge(s, n) THEN "verify" ELSE mstep

\* The review of a part ends disputed or at its round limit: blocked{review}.
ReviewBlock(p) ==
    /\ st[p] = "reviewing" /\ fails < MaxFail
    /\ st' = [st EXCEPT ![p] = "blocked"] /\ held' = [held EXCEPT ![p] = "review"] /\ fails' = fails + 1
    /\ UNCHANGED <<q, approved, gone, mstep, intent, tip, cleaning, restarts>>

Approve(p) ==
    /\ st[p] = "awaiting_approval" /\ ~Halted
    /\ approved' = [approved EXCEPT ![p] = TRUE]
    /\ UNCHANGED <<q, gone, held, intent, tip, cleaning, restarts, fails>>
    /\ LET s == [st EXCEPT ![p] = "queued"]
           n == Advance(s, q)
       IN st' = n /\ mstep' = IF StartsMerge(s, n) THEN "verify" ELSE mstep

\* mergeAndVerify returns (outside h.mu ran the merge and verify).
Verified ==
    /\ mstep = "verify" /\ q # <<>>
    /\ \E ok \in BOOLEAN :
        /\ (~ok => fails < MaxFail)
        /\ fails' = IF ok THEN fails ELSE fails + 1
        /\ IF ok
           THEN /\ intent' = SaveIntent /\ mstep' = "move"
                /\ UNCHANGED <<st, approved, held>>
           ELSE /\ st' = [st EXCEPT ![Front] = "blocked"] /\ approved' = [approved EXCEPT ![Front] = FALSE]
                /\ held' = [held EXCEPT ![Front] = "verify"]
                /\ mstep' = "none" /\ UNCHANGED intent
    /\ UNCHANGED <<q, gone, tip, cleaning, restarts>>

\* git update-ref, outside h.mu: it moves the branch unless the tip moved.
Move ==
    /\ mstep = "move"
    /\ \E ok \in BOOLEAN :
        /\ (~ok => fails < MaxFail)
        /\ fails' = IF ok THEN fails ELSE fails + 1
        /\ tip' = IF ok THEN tip + 1 ELSE tip
        /\ mstep' = IF ok THEN "moved" ELSE "refused"
    /\ UNCHANGED <<st, q, approved, gone, held, intent, cleaning, restarts>>

\* mergePart takes h.mu again: completeMerge, the cleanup, advanceQueue.
Completed ==
    /\ mstep \in {"moved", "refused"} /\ q # <<>>
    /\ intent' = FALSE /\ mstep' = "none"
    /\ IF mstep = "moved"
       THEN LET s == [st EXCEPT ![Front] = "merged"]
                rest == Tail(q)
            IN /\ q' = rest
               /\ cleaning' = (cleaning \/ DueIn(s))
               /\ approved' = approved /\ held' = held
               /\ st' = Advance(s, rest)
       ELSE /\ st' = [st EXCEPT ![Front] = "blocked"] /\ approved' = [approved EXCEPT ![Front] = FALSE]
            /\ held' = [held EXCEPT ![Front] = "queue"]
            /\ UNCHANGED <<q, cleaning>>
    /\ UNCHANGED <<gone, tip, restarts, fails>>

\* A second merge after Completed: advanceQueue may start the next head.
\* (Completed starts it through Advance; its goroutine begins here.)
NextMerge ==
    /\ mstep = "none" /\ \E p \in Parts : st[p] = "merging"
    /\ ~intent
    /\ mstep' = "verify"
    /\ UNCHANGED <<st, q, approved, gone, held, intent, tip, cleaning, restarts, fails>>

\* The cleanup goroutine removes merged worktrees outside h.mu, then takes
\* h.mu, ends and runs advanceQueue.
CleanupStart ==
    /\ ~cleaning /\ CleanupDue
    /\ cleaning' = TRUE
    /\ UNCHANGED <<st, q, approved, gone, held, mstep, intent, tip, restarts, fails>>

Cleaned ==
    /\ cleaning
    /\ gone' = [p \in Parts |-> gone[p] \/ st[p] = "merged"]
    /\ cleaning' = FALSE
    /\ UNCHANGED <<q, approved, held, mstep, intent, tip, restarts, fails>>
    /\ st' = Advance(st, q)

\* The person retries a blocked part (RetryPart). verify: the part leaves the
\* queue for a fix, a new commit and a new review ("reviewing" stands for that
\* path). A disputed review or a restart in review: the review runs again.
\* A conflict or a lost worktree: back to the queue.
Retry(p) ==
    /\ st[p] = "blocked"
    /\ UNCHANGED <<approved, gone, held, intent, tip, cleaning, restarts, fails>>
    /\ LET rest == IF held[p] = "verify" THEN SelectSeq(q, LAMBDA x : x # p) ELSE q
           s == [st EXCEPT ![p] = IF held[p] = "queue" THEN "queued" ELSE "reviewing"]
           n == Advance(s, rest)
       IN /\ q' = rest /\ st' = n
          /\ mstep' = IF StartsMerge(s, n) THEN "verify" ELSE mstep

\* The person waives a blocked review (ResolvePart): the part joins the queue.
Resolve(p) ==
    /\ st[p] = "blocked" /\ held[p] = "review"
    /\ q' = Append(q, p)
    /\ UNCHANGED <<approved, gone, held, intent, tip, cleaning, restarts, fails>>
    /\ LET s == [st EXCEPT ![p] = "queued"]
           n == Advance(s, q')
       IN st' = n /\ mstep' = IF StartsMerge(s, n) THEN "verify" ELSE mstep

Abandon(p) ==
    /\ st[p] \in {"reviewing", "queued", "awaiting_approval", "blocked"}
    /\ UNCHANGED <<approved, gone, held, intent, tip, cleaning, restarts, fails>>
    /\ LET rest == SelectSeq(q, LAMBDA x : x # p)
           s == [st EXCEPT ![p] = "abandoned"]
           n == Advance(s, rest)
       IN /\ q' = rest /\ st' = n
          /\ mstep' = IF StartsMerge(s, n) THEN "verify" ELSE mstep

\* The server stops and starts again: goroutines vanish, recoverWork runs.
Restart ==
    /\ restarts < MaxRestart
    /\ restarts' = restarts + 1
    /\ mstep' = "none" /\ cleaning' = FALSE /\ intent' = FALSE
    /\ LET merging == {p \in Parts : st[p] = "merging"}
           \* With the intent on disk the tip says whether the ref moved.
           landed == intent /\ tip = Cardinality(Merged) + 1
           \* A review in progress is lost too: blocked{restart} (recoverWork).
           s == [p \in Parts |-> IF p \in merging THEN (IF landed THEN "merged" ELSE "blocked")
                                ELSE IF st[p] = "reviewing" THEN "blocked" ELSE st[p]]
           rest == IF landed THEN Tail(q) ELSE q
       IN /\ q' = rest
          /\ held' = [p \in Parts |-> IF p \in merging /\ ~landed THEN "queue"
                                     ELSE IF st[p] = "reviewing" THEN "restart" ELSE held[p]]
          \* resumeRecoveredWork: advanceQueue now, or after the cleanup it starts.
          /\ st' = IF DueIn(s) THEN s ELSE Advance(s, rest)
    /\ UNCHANGED <<approved, gone, tip, fails>>

Step ==
    \/ \E p \in Parts : Pass(p) \/ ReviewBlock(p) \/ Approve(p) \/ Retry(p) \/ Resolve(p) \/ Abandon(p)
    \/ Verified \/ Move \/ Completed \/ NextMerge \/ CleanupStart \/ Cleaned \/ Restart

Halt == ~ENABLED Step /\ UNCHANGED vars

Next == Step \/ Halt

Spec == Init /\ [][Next]_vars /\ WF_vars(Verified) /\ WF_vars(Move) /\ WF_vars(Completed)
             /\ WF_vars(NextMerge) /\ WF_vars(CleanupStart) /\ WF_vars(Cleaned)

-------------------------------------------------------------------------------
\* Properties

TypeOK ==
    /\ st \in [Parts -> {"reviewing", "queued", "awaiting_approval", "merging", "merged", "blocked", "abandoned"}]
    /\ mstep \in {"none", "verify", "move", "moved", "refused"}

\* One part merges at a time.
OneMerge == Cardinality({p \in Parts : st[p] = "merging"}) <= 1

\* A merge and the worktree cleanup never run together.
MergeOrCleanup == ~(cleaning /\ \E p \in Parts : st[p] = "merging")

\* Once no merge is in flight, the integration branch holds one commit per
\* merged part: no merge lost and none counted twice, also across a restart.
TipMatchesMerged == (mstep = "none" /\ ~intent /\ \A p \in Parts : st[p] # "merging") => tip = Cardinality(Merged)

\* The branch moves only while its intent is on disk.
MovesOnlyWithIntent == [][tip' > tip => intent]_vars

\* No merge starts while a part is blocked.
NoMergeWhileHalted == [][StartsMerge(st, st') => \A p \in Parts : st'[p] # "blocked"]_vars

\* A started merge and a started cleanup finish.
MergeEnds == (mstep # "none") ~> (mstep = "none")
CleanupEnds == cleaning ~> ~cleaning

=============================================================================
