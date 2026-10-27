package ui

import (
	"context"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

func TestBuildSession_CrystallizeFileSkipsDuplicateWrites(t *testing.T) {
	ctx := context.Background()
	b := NewBuildSession(icx.NewLocalLattice(), t.TempDir())
	if err := b.Open(ctx, "dedupe fixture", nil); err != nil {
		t.Fatal(err)
	}
	body := "package fixture\nfunc A() int { return 1 }\n"

	seq1, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", body, "")
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", body, "")
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != seq2 {
		t.Errorf("identical content must not re-write to ICX: seq %d then %d", seq1, seq2)
	}
	if got := b.varInt(ctx, "build.file_seq"); got != seq1 {
		t.Errorf("file_seq advanced on a duplicate write: %d", got)
	}

	seq3, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", body+"func B() {}\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if seq3 <= seq1 {
		t.Errorf("changed content must be written: seq %d then %d", seq1, seq3)
	}

	// Reverting to the previous content after a change is a real change and must be stored again.
	seq4, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", body, "")
	if err != nil {
		t.Fatal(err)
	}
	if seq4 <= seq3 {
		t.Errorf("reverted content must be written: seq %d then %d", seq3, seq4)
	}
}

func TestBuildSession_PurgeThenRewriteIsStored(t *testing.T) {
	ctx := context.Background()
	b := NewBuildSession(icx.NewLocalLattice(), t.TempDir())
	if err := b.Open(ctx, "purge fixture", nil); err != nil {
		t.Fatal(err)
	}
	body := "package fixture\nfunc A() int { return 1 }\n"
	seq1, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", body, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PurgeSource(ctx, "project:pkg/fixture/a.go"); err != nil {
		t.Fatal(err)
	}
	seq2, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", body, "")
	if err != nil {
		t.Fatal(err)
	}
	if seq2 <= seq1 {
		t.Errorf("identical content after a purge must be stored again: seq %d then %d", seq1, seq2)
	}
}
