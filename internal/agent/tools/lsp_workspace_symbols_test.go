package tools

import "testing"

func TestWorkspaceSymbolRank(t *testing.T) {
	if symbolRank("FindThing", "findthing") != 0 || symbolRank("FindThing", "find") != 1 || symbolRank("OtherFindThing", "find") != 2 {
		t.Fatal("workspace-symbol ranking must prefer exact, then prefix, then contains matches")
	}
}
