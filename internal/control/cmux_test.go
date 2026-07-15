package control

import "testing"

func TestCmuxResumeCmd(t *testing.T) {
	got := cmuxResumeCmd("surface:2", "hello world")
	want := []string{"cmux", "send", "--surface", "surface:2", "--", "hello world\n"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got[len(got)-1][len(got[len(got)-1])-1] != '\n' {
		t.Errorf("last argv element must end in \\n (Enter), got %q", got[len(got)-1])
	}
}

func TestParseCmuxTree_NestedFixture(t *testing.T) {
	// TODO: verify cmux tree --json shape on a machine with the cmux CLI;
	// this fixture is a guess at plausible shape, the parser is tolerant of
	// variations.
	fixture := []byte(`{
		"windows": [
			{
				"name": "w1",
				"panes": [
					{"kind": "surface", "surfaceId": "surface:2", "cwd": "/Users/imac/IdeaProjects/aboard"},
					{"kind": "pane", "id": "pane:1"}
				]
			}
		]
	}`)

	targets := parseCmuxTree(fixture)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(targets), targets)
	}
	got := targets[0]
	if got.Backend != "cmux" || got.ID != "surface:2" || got.Cwd != "/Users/imac/IdeaProjects/aboard" {
		t.Errorf("target = %+v, want {cmux surface:2 /Users/imac/IdeaProjects/aboard}", got)
	}
}

func TestParseCmuxTree_AltKeyNames(t *testing.T) {
	// alternate key spellings (surface_id / working_directory) must also match.
	fixture := []byte(`{"nodes":[{"surface_id":"surface:9","working_directory":"/tmp/x"}]}`)

	targets := parseCmuxTree(fixture)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(targets), targets)
	}
	if targets[0].ID != "surface:9" || targets[0].Cwd != "/tmp/x" {
		t.Errorf("target = %+v, want {surface:9 /tmp/x}", targets[0])
	}
}

func TestParseCmuxTree_UnknownShape(t *testing.T) {
	if targets := parseCmuxTree([]byte(`{"foo":"bar"}`)); len(targets) != 0 {
		t.Errorf("got %d targets, want 0", len(targets))
	}
	if targets := parseCmuxTree([]byte(`not json`)); targets != nil {
		t.Errorf("unparseable input: got %+v, want nil", targets)
	}
	if targets := parseCmuxTree([]byte(`[]`)); len(targets) != 0 {
		t.Errorf("empty array: got %d targets, want 0", len(targets))
	}
}
