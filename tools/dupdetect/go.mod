// dupdetect is a nested module on purpose: it is repo automation, not part of
// the shipped fleetops binary, so it must never widen the root go.mod (which
// is deliberately thin — see CONTRIBUTING.md). Nothing here may import a
// third-party package; stdlib only.
module github.com/jitokim/fleetops/tools/dupdetect

go 1.25.0
