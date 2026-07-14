# missionctl

**Mission control for a fleet of autonomous agent loops.**
You watch the *gates*, not the *work*.

```
missionctl
```

Loops run themselves. An independent **oracle** verifies that "done" is actually
done (the agent's self-report is never trusted). **Governance** (budget /
max-cycles / no-improve) stops a runaway. A **human gate** blocks at the decisions
only you should own — approve, redirect, or kill, from a terminal fleet console.

> The novel bit isn't running an agent — it's running *many* long loops without
> them lying, drifting, or running away, with a human on the gates. See
> [`DESIGN.md`](./DESIGN.md).

## Status

`0.1` — design + scaffold. Phase 0 (single-process MVP: engine + oracle + governor
+ Textual TUI + gate-approve) is in progress. Not usable yet.

## Layout

```
src/missionctl/
├── domain/     # ports (Agent/Oracle/Challenger/GatePolicy/LoopStore), states, models
├── engine/     # LoopEngine (the convergent state machine) + Governor
├── store/      # persistence (JSON → SQLite)
├── adapters/   # concrete Agent/Oracle impls (fakes now; LLM-via-Bifrost next)
└── tui/        # Textual fleet console (the mockup)
```

## Dev

```bash
python -m venv .venv && . .venv/bin/activate
pip install -e ".[dev]"
python -m missionctl        # launch the TUI (sample fleet for now)
pytest -q
```

License: intended Apache-2.0 (OSS).
