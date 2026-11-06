package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
)

const (
	InputSending   = "sending"   // persisted before writing to the provider
	InputAccepted  = "accepted"  // provider acknowledged this exact input
	InputRejected  = "rejected"  // provider did not accept it; queue the original
	InputUncertain = "uncertain" // a write may have succeeded; inspect history first
)

type SteerInput struct {
	ID     string
	Text   string
	Images []Image
}

type SteerReceipt struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	SessionID string `json:"session_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"` // provider turn, not the room turn
	ClientID  string `json:"client_id,omitempty"`
	Source    string `json:"source,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// SteerResolver reads persisted provider history without starting inference.
// It is used after the sending process has exited, including server recovery.
type SteerResolver interface {
	ResolveSteer(context.Context, SteerReceipt) (SteerReceipt, error)
}

func emitReceipt(emit func(Event), receipt SteerReceipt) {
	x := ev(KindDelivery)
	x.ID, x.Receipt = receipt.ID, &receipt
	emit(x)
}

// Claude requires a UUID on an input. Derive it from the durable room message
// ID so recovery can identify it without allocating another id on a retry.
func claudeInputID(id string) string {
	b := sha256.Sum256([]byte("roundtable/input/" + id))
	b[6] = (b[6] & 15) | 0x50
	b[8] = (b[8] & 63) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
