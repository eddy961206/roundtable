package room

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"roundtable/internal/agent"
)

const (
	deliveryPending   = "pending"   // accepted by rt, not yet written to a CLI
	deliveryQueued    = "queued"    // next ordinary turn
	deliveryDelivered = "delivered" // ordinary prompt was delivered
	deliveryError     = "error"     // an ordinary delivery turn visibly failed
	deliveryStopped   = "stopped"   // an ordinary delivery turn was cancelled
	deliveryFailed    = "failed"    // delivery could not be established; visible, never silently replayed
	inputRecoveryKey  = "input_delivery"
)

// InputDelivery is durable per-message state. TurnID is the room's turn;
// ProviderTurnID is Codex's turn. Keeping both avoids guessing an older reply.
type InputDelivery struct {
	State          string `json:"state"`
	TurnID         string `json:"turn_id,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	ProviderTurnID string `json:"provider_turn_id,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
	Source         string `json:"source,omitempty"`
	Detail         string `json:"detail,omitempty"`
	Halted         bool   `json:"halted,omitempty"` // an explicit stop must survive a restart
}

func (h *Hub) steerInput(rs *roomState, m *Message) agent.SteerInput {
	input := agent.SteerInput{ID: m.ID, Text: m.Text}
	for _, a := range m.Attachments {
		path, _ := filepath.Abs(h.store.attachmentPath(rs.room.ID, a))
		input.Images = append(input.Images, agent.Image{Path: path, Mime: a.Mime})
	}
	return input
}

func canSteerImages(prov agent.Provider) bool {
	p, ok := prov.(interface{ CanSteerImages() bool })
	return ok && p.CanSteerImages()
}

func (h *Hub) saveDelivery(rs *roomState, m *Message, d InputDelivery) error {
	m.Delivery = &d
	m.Live = d.State == agent.InputAccepted
	if err := h.store.AppendMessage(rs.room.ID, m); err != nil {
		return err
	}
	rs.broadcast("message", m)
	return nil
}

// recordSteer is called under h.mu before the trace is written. In particular,
// sending is durable before the provider is allowed to write the input.
func (h *Hub) recordSteer(rs *roomState, run *running, receipt agent.SteerReceipt) (bool, error) {
	if run == nil {
		return false, nil
	}
	for _, m := range run.live {
		if m.ID != receipt.ID || m.Delivery == nil || m.Delivery.TurnID != run.turn.ID {
			continue
		}
		d := *m.Delivery
		if d.State == agent.InputAccepted && receipt.State != agent.InputAccepted {
			return false, nil
		}
		switch receipt.State {
		case agent.InputSending, agent.InputAccepted, agent.InputUncertain:
			d.State = receipt.State
		case agent.InputRejected:
			d.State = deliveryQueued
		default:
			return false, fmt.Errorf("알 수 없는 입력 수신 상태: %s", receipt.State)
		}
		d.SessionID = receipt.SessionID
		if d.SessionID == "" {
			d.SessionID = run.msg.SessionID
		}
		d.ProviderTurnID, d.ClientID, d.Source, d.Detail = receipt.TurnID, receipt.ClientID, receipt.Source, receipt.Detail
		if d == *m.Delivery {
			return false, nil
		}
		return true, h.saveDelivery(rs, m, d)
	}
	return false, nil // a late receipt from a different invocation cannot consume an input
}

func resolveReceipt(prov agent.Provider, receipt agent.SteerReceipt) (agent.SteerReceipt, error) {
	resolver, ok := prov.(agent.SteerResolver)
	if !ok {
		return receipt, fmt.Errorf("이 공급사는 저장된 입력의 수신 여부 조회를 지원하지 않음")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return resolver.ResolveSteer(ctx, receipt)
}

// The provider has already closed its process tree when this runs. A timeout
// is not a rejection: use the durable client id before deciding to replay.
func resolveRunReceipts(prov agent.Provider, res *agent.Result, emit func(agent.Event)) {
	for i, receipt := range res.Receipts {
		if receipt.State == agent.InputSending {
			receipt.State = agent.InputUncertain
		}
		if receipt.SessionID == "" {
			receipt.SessionID = res.SessionID
		}
		if receipt.State == agent.InputUncertain {
			resolved, err := resolveReceipt(prov, receipt)
			if err == nil {
				receipt = resolved
			} else {
				receipt.Detail = "전달 확인 실패: " + err.Error()
			}
		}
		res.Receipts[i] = receipt
		copy := receipt
		emit(agent.Event{TS: nowMs(), Kind: agent.KindDelivery, ID: receipt.ID, Receipt: &copy})
	}
}

func (h *Hub) settleSteers(rs *roomState, pr *prepared, run *running) error {
	for _, m := range run.live {
		if m.Delivery == nil || m.Delivery.State == agent.InputAccepted {
			continue
		}
		d := *m.Delivery
		if pr.stopGen != rs.stopGen || run.discardFollowups || !pr.p.Enabled {
			d.Halted = true
		}
		switch d.State {
		case deliveryPending:
			d.State, d.Detail = deliveryQueued, "현재 턴이 입력을 받기 전에 끝남"
		case agent.InputSending, agent.InputUncertain:
			d.State = deliveryFailed
			if d.Detail == "" {
				d.Detail = "수신 여부를 확정하지 못함; 원문을 보존함"
			}
		}
		if err := h.saveDelivery(rs, m, d); err != nil {
			return err
		}
		if d.State == deliveryQueued && !d.Halted {
			rs.enqueueFront(&Turn{ID: newID(), Agent: pr.p.ID, Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
		}
	}
	return nil
}

func (h *Hub) finishPromptInputs(rs *roomState, pr *prepared, delivered bool) error {
	for _, m := range pr.history {
		if m.Target != pr.p.ID || m.Delivery == nil || (m.Delivery.State != deliveryQueued && m.Delivery.State != deliveryError && m.Delivery.State != deliveryStopped) {
			continue
		}
		d := *m.Delivery
		if !delivered && pr.msg.Status != "error" && pr.msg.Status != "cancelled" {
			continue
		}
		d.State, d.TurnID, d.SessionID, d.Halted = deliveryDelivered, pr.t.ID, pr.msg.SessionID, false
		d.ProviderTurnID, d.Source, d.Detail = "", "prompt", ""
		if !delivered {
			d.State, d.Detail = deliveryError, pr.msg.Error
			if pr.msg.Status == "cancelled" {
				d.State = deliveryStopped
			}
		}
		if err := h.saveDelivery(rs, m, d); err != nil {
			return err
		}
	}
	return nil
}

// An explicit stop is durable, even if the server dies before Run returns.
func (h *Hub) haltDeliveries(rs *roomState, run *running) error {
	for _, m := range run.live {
		if m.Delivery == nil {
			continue
		}
		d := *m.Delivery
		d.Halted = true
		if err := h.saveDelivery(rs, m, d); err != nil {
			return err
		}
	}
	return nil
}

func deliveryReceipt(m *Message) agent.SteerReceipt {
	d := m.Delivery
	return agent.SteerReceipt{ID: m.ID, State: agent.InputUncertain, SessionID: d.SessionID, TurnID: d.ProviderTurnID, ClientID: d.ClientID, Source: d.Source, Detail: d.Detail}
}

func deliveryAlreadySeen(rs *roomState, m *Message, p *Participant) bool {
	last, holes := p.LastSeen, p.Unseen
	if m.Part != "" && rs.room.Work != nil {
		if part := rs.room.Work.part(m.Part); part != nil {
			last, holes = part.LastSeen, part.Unseen
		}
	}
	return m.Seq <= last && !slices.Contains(holes, m.Seq)
}

func needsInputRecovery(m *Message) bool {
	if m.Kind != "user" || m.Delivery == nil {
		return false
	}
	switch m.Delivery.State {
	case deliveryPending, agent.InputSending, agent.InputUncertain, deliveryQueued:
		return true
	}
	return false
}

func (h *Hub) stageInputRecovery() {
	for _, rs := range h.rooms {
		if slices.ContainsFunc(rs.msgs, needsInputRecovery) {
			rs.workBusy[inputRecoveryKey] = true
		}
	}
}

// Called after all rooms have been loaded. Legacy Live-only records are left
// unchanged. New pending/sending records never remain stuck after a crash.
func (h *Hub) recoverInputs() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rs := range h.rooms {
		var pending []*Message
		for _, m := range rs.msgs {
			if needsInputRecovery(m) {
				pending = append(pending, m)
			}
		}
		if len(pending) == 0 {
			continue
		}
		rs.workBusy[inputRecoveryKey] = true
		gen := rs.stopGen
		h.turnWG.Add(1)
		go h.recoverRoomInputs(rs, pending, gen)
	}
}

func (h *Hub) recoverRoomInputs(rs *roomState, inputs []*Message, gen int) {
	defer h.turnWG.Done()
	for _, m := range inputs {
		h.mu.Lock()
		p := rs.room.participant(m.Target)
		if rs.deleted || p == nil || p.Removed {
			h.mu.Unlock()
			continue
		}
		d := *m.Delivery
		prov := h.reg.Get(p.Provider)
		seen := deliveryAlreadySeen(rs, m, p)
		storedReceipt := deliveryReceipt(m)
		h.mu.Unlock()
		switch d.State {
		case deliveryPending:
			d.State, d.Detail = deliveryQueued, "서버 재시작: 공급사에 쓰기 전 입력을 다음 턴으로 전달"
		case deliveryQueued:
			if seen {
				d.State = deliveryDelivered
			}
		case agent.InputSending, agent.InputUncertain:
			receipt, err := resolveReceipt(prov, storedReceipt)
			if err != nil {
				d.State, d.Detail = deliveryFailed, "서버 재시작 뒤 전달 확인 실패: "+err.Error()
			} else {
				d.Source, d.Detail = receipt.Source, receipt.Detail
				switch receipt.State {
				case agent.InputAccepted:
					d.State = agent.InputAccepted
				case agent.InputRejected:
					d.State = deliveryQueued
				default:
					d.State, d.Detail = deliveryFailed, "저장된 입력의 수신 여부를 확정하지 못함"
				}
			}
		}
		h.mu.Lock()
		if !rs.deleted {
			if gen != rs.stopGen || h.closing || !p.Enabled || m.Delivery.Halted {
				d.Halted = true
			}
			if err := h.saveDelivery(rs, m, d); err != nil {
				h.reportPersistenceError(rs, "입력 전달 복구", err)
			} else if d.State == deliveryQueued && !d.Halted {
				rs.enqueueFront(&Turn{ID: newID(), Agent: p.ID, Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
			}
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(rs.workBusy, inputRecoveryKey)
	if !rs.deleted {
		h.saveRoom(rs)
		h.pump(rs)
		rs.publishList()
	}
}
