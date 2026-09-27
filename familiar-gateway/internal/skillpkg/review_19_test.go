package skillpkg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A personal skill and a library skill of the same name can't both be
// bound to a shard (the prompt listed the name twice and use_skill
// served either), and one bound earlier is served deterministically:
// the owner's.
func TestStore_ShardSkillsOneNameEach(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	alice, shard := uniqueName("alice"), uniqueName("shard")
	seedUserAndShard(t, s, alice, shard)
	name := uniqueName("dup")
	lib, err := s.ImportZip(ctx, zipSkill(t, name, false), alice, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := s.SaveAuthored(ctx, alice, name, "mine", "# Mine\n\nMINEBODY", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetShardSkills(ctx, shard, []string{lib.ID, mine.ID}); err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("binding both %q = %v, want refused", name, err)
	}
	// Bound the old way (both rows): one listed, the owner's served.
	for _, id := range []string{lib.ID, mine.ID} {
		if _, err := s.pool.ExecContext(ctx, `INSERT INTO shard_skills (shard_id, skill_id) VALUES ($1, $2::uuid) ON CONFLICT DO NOTHING`, shard, id); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := s.ListShardSkills(ctx, shard)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != mine.ID {
		t.Errorf("listed %d (%v), want only alice's", len(listed), listed)
	}
	for i := 0; i < 5; i++ {
		p, err := s.boundPackage(ctx, shard, name)
		if err != nil || p.ID != mine.ID {
			t.Fatalf("boundPackage = %v, %v; want alice's every time", p, err)
		}
	}
}

// Two creates of one name at once (a double-clicked Save): one wins,
// the other waits and is refused, and the winner's files survive. The
// loser's cleanup deleted them.
func TestStore_ConcurrentCreateKeepsTheWinnersFiles(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	alice := uniqueName("alice")
	seedUserAndShard(t, s, alice, uniqueName("shard"))
	name := uniqueName("twice")
	// Every create pauses after its name check, so without the lock all
	// of them pass it before any inserts.
	afterNameLookup = func() { time.Sleep(50 * time.Millisecond) }
	t.Cleanup(func() { afterNameLookup = nil })
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.CreateAuthored(ctx, alice, name, "d", "# B\n\nBODY", nil)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrExists):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d creates succeeded, want 1", ok)
	}
	if _, err := os.Stat(filepath.Join(s.Root, userSubdir, alice, name, "SKILL.md")); err != nil {
		t.Errorf("the skill's files are gone: %v", err)
	}
	// Save (not create) still updates.
	if _, err := s.SaveAuthored(ctx, alice, name, "d2", "# B\n\nBODY2", nil); err != nil {
		t.Errorf("update after create: %v", err)
	}
}
