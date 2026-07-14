"""`missionctl` / `python -m missionctl` → launch the fleet console."""

from __future__ import annotations


def main() -> None:
    from missionctl.tui.app import run
    run()


if __name__ == "__main__":
    main()
