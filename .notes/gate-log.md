# Gate Log

## 2026-07-17 — feat/inject-headless-exact-fallback — 휴먼게이트 요청 (PENDING)

- **요청 시각:** 2026-07-17 (KST 밤)
- **상태:** 캡틴 승인 대기 — 승인/반려 발화가 이 파일에 기록되기 전까지
  커밋정리·PR 단계로 진행하지 않는다.
- **제시 내용:** 무엇을(ambiguous Idle/Stalled inject를 Tier 2 headless-exact로
  fallback) · 왜(orca 멀티세션 데드엔드 해소, 리서치 문서 참조) ·
  어떻게(allowlist 게이트 + submit 재검증 + 정직한 메시징, 라우팅 무변경) ·
  남은 리스크(아래) 요약을 세션에서 캡틴에게 제시.
- **남은 리스크 (캡틴에게 고지):**
  1. Stalled 판정이 오탐(실제로는 진행 중)일 때 headless 턴이 세션에 추가될
     수 있음 — 단 429 auto-redrive가 이미 동일 리스크를 안고 무조건
     구동 중이며, resume 재전송은 무해한 no-op 턴 (README 설계 근거 준용).
  2. README Tier 1a "tmux only" 서술은 PR #35(cmux LocateByTTY) 이후 stale —
     이 브랜치 범위 밖(surgical), 별도 docs 후속으로 제안.
- **챌린저 통과:** 1) 시니어 엔지니어 — research 문서에 기록.
  2) 리뷰어(반려 포인트 3개 선제 수정) — ① 타이핑 중 Idle→Running 레이스에
  submit 시점 재검증 부재 → 재검증 추가, ② interim 상태 메시지가 headless
  라우팅인데 in-place처럼 표시 → 정직화, ③ README Tier 1b 미동기화 → 동기화.
