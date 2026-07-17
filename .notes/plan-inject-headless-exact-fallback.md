# Plan — inject headless-exact fallback

기준: `.notes/research-inject-headless-exact-fallback.md` (2026-07-17)
브랜치: `feat/inject-headless-exact-fallback`

## Scope

1. `i` keypress 가드: ambiguous(공유 cwd, tty 없음) 타깃이라도 상태가
   **Idle 또는 Stalled**면 거절하지 않고 inject 모드 진입 허용.
   라우팅은 기존 `sendPromptCmd`의 Tier1→Tier2 폴스루 그대로 —
   `claude --resume <정확한 SessionID> -p <prompt>` (오배송 구조적 불가).
2. Eligibility는 **명시적 allowlist** (`injectHeadlessFallbackEligible`):
   Idle/Stalled만 true, 그 외 전부(미래의 미지 상태 포함) fail-closed.
   Running은 절대 불가 — 라이브 턴과의 동시 headless resume은 미검증 영역
   (ADR §4), 트랜스크립트 충돌/포크 위험.
3. **Submit 시점 재검증**: 타이핑 중 상태가 바뀔 수 있으므로(fleet refresh는
   modeInjecting 중에도 흐름) Enter 시점에 현재 플릿 스냅샷에서 SessionID로
   같은 세션을 재조회해 동일 가드 재실행. Idle→Running 전환 시 거절.
4. **정직한 메시징**: Tier 2 다운그레이드 결과 메시지에 정확히 어느 세션에
   배달됐는지(shortID) + "background turn, 열린 창에는 안 보임" 명시.
   Submit 직후 interim 상태도 headless 라우팅임을 표시.
5. README Tier 1b 서술 동기화.

## Non-goals

- StateGate / StateDrift / StatePaused / Terminal 상태들의 fallback — 각자
  고유 의미론이 있고(DRIFT는 `r`의 힌트 재구동 플로 보유) 요청된 적 없음.
  침묵 포함이 아니라 **명시적 제외** (allowlist 주석에 문서화).
- 새 라우팅 메커니즘/백엔드 변경 — Tier 정책 자체는 무변경.
- ADR 본문 개정 — ADR은 결정 기록; 이 기능은 ADR의 fail-closed 원칙을
  따르는 확장이며 PR 본문에서 참조만 한다.

## 협력 구조 (책임·메시지 흐름)

관여 객체 3+: `Model`(TUI 상태기) · `domain.Loop`/`LoopState`(도메인) ·
`sendPromptCmd`/`control.Redrive`(액추에이션).

- `Model."i" 핸들러`: keypress 시점 fail-fast 게이트 (인간이 프롬프트를 다
  치고 나서 거절당하지 않게). 판단은 `injectHeadlessFallbackEligible`(순수
  함수, domain.LoopState → bool)에 위임.
- `Model.enter 핸들러`: dispatch 직전 최종 게이트 (같은 가드를 fresh 상태로
  재실행 — belt-and-suspenders, 기존 actuating 재확인과 동일 규율).
- `sendPromptCmd`: 라우팅 책임 단독 보유 (Tier1 시도 → 실패 시 Tier 2
  headless-exact). 이번 변경에서 라우팅 로직 무변경, 메시지만 정직화.

## 검증 방법

- 유닛: `injectHeadlessFallbackEligible` 전 상태(9개 + 가상 미래 상태)
  table-driven — fail-closed 증명.
- 통합(성공): Stalled/Idle ambiguous → inject 모드 진입 → 정확한 SessionID로
  Tier 2 라우팅 full round-trip (orca 3세션 1워크트리 픽스처 포함).
- 통합(실패/음성): Running ambiguous 거절(redrive mock이 호출되면 즉시 실패
  하는 defense-in-depth), Tier 1 해소 가능 타깃은 여전히 in-place,
  타이핑 중 Idle→Running 레이스 거절, 빈 프롬프트 취소.
- 실기능: `fleetops --demo` 구동 렌더 확인 + 전 패키지 `go test -race`.
