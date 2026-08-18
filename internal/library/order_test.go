package library

import (
	"testing"

	"tsukimi/internal/domain"
	"tsukimi/internal/store"
)

// TestListOrderNewestFirst 验证书库列表按 Order 降序返回（最新入库在前），
// 与 JM 收藏 API 的默认顺序一致（最新在前）。
func TestListOrderNewestFirst(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := New(dir, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 按远端收藏顺序逐条 Upsert：A 最早、C 最新。
	for _, id := range []string{"AAA", "BBB", "CCC"} {
		if err := svc.Upsert(domain.Manga{SourceID: "jmcomic", ID: id, Title: "t-" + id}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	got := svc.List()
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	// 最新入库（CCC）应在最前；最早入库（AAA）在最后。
	want := []string{"CCC", "BBB", "AAA"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("List[%d].ID = %q, want %q (full order: %v)", i, got[i].ID, w, idsOf(got))
		}
	}
}

// TestListOrderStableAfterReload 验证重启后续号、顺序稳定。
func TestListOrderStableAfterReload(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := New(dir, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, id := range []string{"AAA", "BBB"} {
		if err := svc.Upsert(domain.Manga{SourceID: "jmcomic", ID: id, Title: "t-" + id}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	// 重启（同一 store/目录）
	st2, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New reload: %v", err)
	}
	svc2, err := New(dir, st2)
	if err != nil {
		t.Fatalf("New reload: %v", err)
	}
	// 新增一本
	if err := svc2.Upsert(domain.Manga{SourceID: "jmcomic", ID: "CCC", Title: "t-CCC"}); err != nil {
		t.Fatalf("upsert CCC: %v", err)
	}
	got := svc2.List()
	want := []string{"CCC", "BBB", "AAA"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("after reload List[%d].ID = %q, want %q (full: %v)", i, got[i].ID, w, idsOf(got))
		}
	}
}

func idsOf(list []domain.Manga) []string {
	out := make([]string, len(list))
	for i, m := range list {
		out[i] = m.ID
	}
	return out
}

// TestReorderByFavorites 验证「按收藏顺序重排」：
//   - 收藏列表里的本地漫画按收藏顺序赋 Order（最新在前）。
//   - 不在收藏列表里的本地漫画排到收藏漫画之后。
func TestReorderByFavorites(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := New(dir, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 本地已有 4 本：A、B 在收藏里；X、Y 不在收藏里（按 ID 下载的）。
	for _, id := range []string{"A", "B", "X", "Y"} {
		if err := svc.Upsert(domain.Manga{SourceID: "jmcomic", ID: id, Title: id}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	// 远端收藏顺序：B 最新、A 最老（只列本地存在的；实际可能夹杂不在本地的）。
	// 同时混入一本本地没有的 Z，验证不会因此出错。
	favOrder := []string{"B", "Z", "A"}
	changed := svc.ReorderByFavorites("jmcomic", favOrder)
	if changed == 0 {
		t.Fatalf("ReorderByFavorites changed=0, want >0")
	}
	got := svc.List()
	// 期望顺序：B（最新收藏）→ A → Y、X（不在收藏里，按入库新→旧排在后面）。
	if got[0].ID != "B" {
		t.Errorf("List[0].ID = %q, want B", got[0].ID)
	}
	if got[1].ID != "A" {
		t.Errorf("List[1].ID = %q, want A", got[1].ID)
	}
	tail := idsOf(got[2:])
	if tail[0] != "Y" || tail[1] != "X" {
		t.Errorf("tail = %v, want [Y X] (不在收藏里的按入库新→旧排)", tail)
	}
	// 重排幂等：再次用同样的顺序调用，changed 应为 0。
	if changed2 := svc.ReorderByFavorites("jmcomic", favOrder); changed2 != 0 {
		t.Errorf("idempotent re-order changed=%d, want 0", changed2)
	}
}
