package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkRun — создаёт каталог прогона с указанным временем модификации и файлом
// внутри, чтобы было что считать и удалять.
func mkRun(t *testing.T, base, id string, mod time.Time, size int) string {
	t.Helper()
	dir := filepath.Join(base, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	// Время модификации каталога задаётся вручную: иначе сортировка «самых
	// свежих» опиралась бы на время создания, а тест должен управлять им сам.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, mod); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestPruneEmptyPolicyRemovesNothing — защита от «позвали prune с пустой
// политикой и потеряли журналы».
func TestPruneEmptyPolicyRemovesNothing(t *testing.T) {
	base := t.TempDir()
	mkRun(t, base, "a", time.Now(), 10)
	mkRun(t, base, "b", time.Now(), 10)
	s := NewFilesystemStore(base)
	res, err := s.Prune(PrunePolicy{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("пустая политика удалила %v", res.Removed)
	}
	ids, _ := s.ListRuns()
	if len(ids) != 2 {
		t.Errorf("осталось %d прогонов, ожидалось 2", len(ids))
	}
}

// TestPruneKeepLastKeepsNewest — остаются самые свежие.
func TestPruneKeepLastKeepsNewest(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	mkRun(t, base, "old", now.Add(-72*time.Hour), 10)
	mkRun(t, base, "mid", now.Add(-48*time.Hour), 10)
	mkRun(t, base, "new", now.Add(-1*time.Hour), 10)

	s := NewFilesystemStore(base)
	res, err := s.Prune(PrunePolicy{KeepLast: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("удалено %v, ожидались два", res.Removed)
	}
	ids, _ := s.ListRuns()
	if len(ids) != 1 || ids[0] != "new" {
		t.Errorf("остались %v, ожидался только new", ids)
	}
}

// TestPruneKeepDaysRemovesOld — старые уходят, свежие остаются.
func TestPruneKeepDaysRemovesOld(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	mkRun(t, base, "ancient", now.Add(-40*24*time.Hour), 10)
	mkRun(t, base, "recent", now.Add(-2*24*time.Hour), 10)

	s := NewFilesystemStore(base)
	res, err := s.Prune(PrunePolicy{KeepDays: 7}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "ancient" {
		t.Fatalf("удалено %v, ожидался ancient", res.Removed)
	}
	ids, _ := s.ListRuns()
	if len(ids) != 1 || ids[0] != "recent" {
		t.Errorf("остались %v, ожидался recent", ids)
	}
}

// TestPruneDryRunDeletesNothing — считает, но не трогает.
func TestPruneDryRunDeletesNothing(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	mkRun(t, base, "a", now.Add(-72*time.Hour), 100)
	mkRun(t, base, "b", now.Add(-1*time.Hour), 100)

	s := NewFilesystemStore(base)
	res, err := s.Prune(PrunePolicy{KeepLast: 1, DryRun: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 {
		t.Errorf("в dry-run ожидался один кандидат, получено %v", res.Removed)
	}
	if res.FreedBytes == 0 {
		t.Error("dry-run должен считать освобождаемое место")
	}
	ids, _ := s.ListRuns()
	if len(ids) != 2 {
		t.Errorf("dry-run удалил прогоны: остались %v", ids)
	}
}

// TestPruneBothLimitsTogether — ограничения применяются вместе.
func TestPruneBothLimitsTogether(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	for i, h := range []int{200, 100, 50, 1} {
		mkRun(t, base, string(rune('a'+i)), now.Add(-time.Duration(h)*time.Hour), 10)
	}
	s := NewFilesystemStore(base)
	// keep-last=2, keep-days=7 → остаться могут только те, что моложе 7 дней
	// и входят в двойку свежих: это 'c' (50ч) и 'd' (1ч).
	res, err := s.Prune(PrunePolicy{KeepLast: 2, KeepDays: 7}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 2 {
		t.Fatalf("удалено %v, ожидались два", res.Removed)
	}
	ids, _ := s.ListRuns()
	if len(ids) != 2 {
		t.Errorf("осталось %v, ожидались два", ids)
	}
}

// TestPruneJsonStoreCleansIndex — индекс runs.db обязан знать то же, что диск.
// Иначе `runs list` показывал бы прогоны, которых уже нет.
func TestPruneJsonStoreCleansIndex(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	s := NewJsonStore(base, "")
	for _, id := range []string{"old", "new"} {
		j, err := s.Create(id)
		if err != nil {
			t.Fatal(err)
		}
		// Журнал держит файл открытым, а на Windows открытый файл не даёт
		// удалить каталог. Закрываем — иначе тест проверял бы не чистку, а
		// блокировку файла.
		j.Close()
	}
	// Время модификации задаём после создания.
	if err := os.Chtimes(filepath.Join(base, "old"), now.Add(-72*time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(base, "new"), now.Add(-1*time.Hour), now.Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Prune(PrunePolicy{KeepLast: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "old" {
		t.Fatalf("удалено %v, ожидался old", res.Removed)
	}
	db, err := s.loadDB()
	if err != nil {
		t.Fatal(err)
	}
	if len(db.Runs) != 1 || db.Runs[0].ID != "new" {
		t.Errorf("в индексе остались %v, ожидался только new", db.Runs)
	}
}

// TestPruneSkipsUnsafeRunID — битый id в каталоге не должен ломать уборку и не
// должен приводить к удалению чего-то за пределами runs.
func TestPruneSkipsUnsafeRunID(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	mkRun(t, base, "good", now.Add(-72*time.Hour), 10)
	// Каталог с именем, которое ValidateRunID не пропустит.
	if err := os.MkdirAll(filepath.Join(base, "..evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "..evil")
	s := NewFilesystemStore(base)
	res, err := s.Prune(PrunePolicy{KeepLast: 0, KeepDays: 1}, now)
	if err != nil {
		t.Fatalf("битый id не должен ломать чистку: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "good" {
		t.Errorf("удалено %v, ожидался good", res.Removed)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("каталог вне runs был удалён: %v", err)
	}
}
