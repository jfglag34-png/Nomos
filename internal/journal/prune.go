package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PrunePolicy — что и сколько хранить на диске.
//
// Нули означают «не ограничивать». Оба ограничения применяются ВМЕСТЕ: раны
// старше KeepDays удаляются, и сверх того остаются только KeepLast самых
// свежих. То есть `--keep-last=10 --keep-days=7` означает «не больше десяти
// ранов и не старше недели».
//
// Политика по умолчанию пустая, и это осознанно: удалять чужие прогоны без
// явного спроса нельзя. Стирание идёт только когда сказано.
type PrunePolicy struct {
	// KeepLast — сколько самых свежих прогонов оставить. 0 = без ограничения.
	KeepLast int
	// KeepDays — сколько дней хранить. 0 = без ограничения.
	KeepDays int
	// DryRun — только посчитать, ничего не удалять.
	DryRun bool
}

// Empty — политика ничего не требует.
func (p PrunePolicy) Empty() bool { return p.KeepLast <= 0 && p.KeepDays <= 0 }

// PruneResult — что произошло.
type PruneResult struct {
	// Removed — id удалённых прогонов, по порядку.
	Removed []string
	// Kept — сколько прогонов осталось.
	Kept int
	// FreedBytes — сколько освобождено (при DryRun это «сколько освободилось
	// бы»).
	FreedBytes int64
}

// runEntry — прогон, как его видит чистильщик.
type runEntry struct {
	id    string
	dir   string
	mtime time.Time
	size  int64
}

// Prune — удаляет прогоны по политике.
//
// Политика по умолчанию ничего не удаляет: без явного KeepLast или KeepDays
// возвращается пустой результат и НИ ОДИН файл не трогается. Это защита от
// сценария «вызвали prune с пустой политикой и потеряли журналы».
//
// Порядок отбраковки: сначала по времени (модифицирован каталог рана), потом по
// количеству. Сортировка по времени идёт по убыванию, так что «самые свежие
// KeepLast» — это первые KeepLast в отсортированном списке.
//
// Каталог рана ищется через SafeRunDir, а не склейкой путей: id прогона
// приходит из индекса, а в индексе он в прошлом уже был недоверенным входом.
func (s *FilesystemStore) Prune(policy PrunePolicy, now time.Time) (PruneResult, error) {
	res := PruneResult{}
	if policy.Empty() {
		return res, nil
	}
	ids, err := s.ListRuns()
	if err != nil {
		return res, err
	}
	var failures []string
	entries := make([]runEntry, 0, len(ids))
	for _, id := range ids {
		dir, err := s.runDir(id)
		if err != nil {
			// Небезопасный id пропускаем: чистить по нему нельзя, а ошибку
			// возвращать нельзя — один битый каталог не должен ломать уборку.
			continue
		}
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		size := dirSize(dir)
		entries = append(entries, runEntry{id: id, dir: dir, mtime: fi.ModTime(), size: size})
	}
	// Свежие сверху.
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.After(entries[j].mtime) })

	cutoff := time.Time{}
	if policy.KeepDays > 0 {
		cutoff = now.Add(-time.Duration(policy.KeepDays) * 24 * time.Hour)
	}
	doomed := map[string]bool{}
	for i, e := range entries {
		if policy.KeepLast > 0 && i >= policy.KeepLast {
			doomed[e.id] = true
			continue
		}
		if !cutoff.IsZero() && e.mtime.Before(cutoff) {
			doomed[e.id] = true
		}
	}
	for _, e := range entries {
		if !doomed[e.id] {
			continue
		}
		res.Removed = append(res.Removed, e.id)
		res.FreedBytes += e.size
		if policy.DryRun {
			continue
		}
		if err := os.RemoveAll(e.dir); err != nil {
			// Один неудаляемый прогон не должен валить всю уборку. На Windows
			// каталог с открытым журналом не удаляется — а журнал открыт у
			// идущего рана. Остальные чистятся, а неудавшийся перечисляется в
			// ошибке в конце.
			//
			// Такой прогон при этом НЕ попадает в Removed: он остался на диске,
			// и называть его удалённым нельзя.
			res.Removed = res.Removed[:len(res.Removed)-1]
			res.FreedBytes -= e.size
			res.Kept++
			failures = append(failures, fmt.Sprintf("%s: %v", e.id, err))
			continue
		}
	}
	res.Kept = len(entries) - len(res.Removed)
	if len(failures) > 0 {
		return res, fmt.Errorf("не удалось убрать %d прогонов: %s",
			len(failures), strings.Join(failures, "; "))
	}
	return res, nil
}

// dirSize — суммарный размер каталога. Считается рекурсивно; ошибки
// игнорируются, потому что это оценка «сколько освободится», а не точность.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Prune — та же чистка для json-хранилища: дополнительно вычищаются записи
// удалённых прогонов из индекса runs.db.
//
// Без этого индекс разрастался бы вопреки удалённым каталогам, и `runs list`
// показывал бы прогоны, которых уже нет на диске.
func (s *JsonStore) Prune(policy PrunePolicy, now time.Time) (PruneResult, error) {
	res, err := s.FilesystemStore.Prune(policy, now)
	if err != nil || policy.DryRun || len(res.Removed) == 0 {
		return res, err
	}
	gone := map[string]bool{}
	for _, id := range res.Removed {
		gone[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.loadDB()
	if err != nil {
		return res, err
	}
	keptRuns := db.Runs[:0]
	for _, r := range db.Runs {
		if !gone[r.ID] {
			keptRuns = append(keptRuns, r)
		}
	}
	db.Runs = keptRuns
	keptEvents := db.Events[:0]
	for _, e := range db.Events {
		if !gone[e.RunID] {
			keptEvents = append(keptEvents, e)
		}
	}
	db.Events = keptEvents
	keptArtifacts := db.Artifacts[:0]
	for _, a := range db.Artifacts {
		if !gone[a.RunID] {
			keptArtifacts = append(keptArtifacts, a)
		}
	}
	db.Artifacts = keptArtifacts
	if err := s.saveDB(db); err != nil {
		return res, err
	}
	return res, nil
}
