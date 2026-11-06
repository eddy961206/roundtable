// Delivery is receipt state, not a claim that the model understood an input.
// Live-only messages predate receipts and retain their historical meaning.
export function deliveryLabel(delivery, legacyLive = false) {
  if (!delivery) return legacyLive ? "현재 턴 전달 · 이전 기록" : "다음 턴 전달";
  if (delivery.halted && !["accepted", "delivered"].includes(delivery.state)) return "중단됨 · 입력 보존";
  switch (delivery.state) {
    case "pending":
    case "sending": return "전송 중";
    case "accepted": return "현재 턴 전달됨";
    case "queued":
    case "rejected": return "다음 턴 대기";
    case "delivered": return "다음 턴 전달됨";
    case "uncertain": return "전달 확인 중";
    case "failed": return "전달 확인 실패";
    case "error": return "전달 턴 실패";
    case "stopped": return "전달 턴 중단";
    default: return "전달 상태 확인 필요";
  }
}
