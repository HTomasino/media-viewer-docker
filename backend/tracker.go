package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// NotificationTracker persists the set of manga chapters and h-manga
// archives that have already had a notification pushed. It is shared
// between Gotify and Discord so each item is only notified once across all
// enabled services.
//
// Stored keys are namespaced by kind so the two kinds cannot collide:
//
//	section|series|chapter            (chapter notification)
//	section|archive|artist|archive    (h-manga archive notification)
//
// On first load after an upgrade from a build without archive support,
// all h-manga chapter entries are purged so they don't suppress legitimate
// future archive notifications for the same artists.
type NotificationTracker struct {
	mu       sync.Mutex
	notified map[string]bool // key: namespaced per kind (see file comment)
	filePath string
}

// NewNotificationTracker creates a tracker backed by filePath, loading any
// previously persisted set from disk.
func NewNotificationTracker(filePath string) *NotificationTracker {
	t := &NotificationTracker{
		notified: make(map[string]bool),
		filePath: filePath,
	}
	t.Load()
	return t
}

// SetFilePath changes the persistence path. If the path changed, the tracker
// reloads the set from the new location so notifications are not re-sent for
// chapters already pushed by a previous install.
func (t *NotificationTracker) SetFilePath(filePath string) {
	if filePath == "" || filePath == t.filePath {
		return
	}
	t.filePath = filePath
	t.Load()
}

// notifiedKey builds a stable key for a chapter: "section|series|chapter"
func notifiedKey(section, series, chapter string) string {
	return fmt.Sprintf("%s|%s|%s", section, series, chapter)
}

// notifiedArchiveKey builds a stable key for an h-manga archive notification:
// "section|archive|artist|archive". The literal "archive" segment namespacing
// keeps archive keys disjoint from chapter keys (chapter series names could
// in theory contain "archive", but the section-segment is fixed and unique
// per kind so the collision space is empty in practice).
func notifiedArchiveKey(section, artist, archive string) string {
	return fmt.Sprintf("%s|archive|%s|%s", section, artist, archive)
}

// Load reads the persisted set of already-notified items from disk.
// h-manga chapter entries from earlier (pre-archive) builds are dropped on
// load to avoid suppressing legitimate archive notifications for the same
// artists after upgrade, and the on-disk file is rewritten if any entries
// were purged so subsequent startups don't repeat the work (and log spam).
func (t *NotificationTracker) Load() {
	t.mu.Lock()
	defer t.mu.Unlock()

	data, err := os.ReadFile(t.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[NOTIFY] Warning: could not read notified-chapters file: %v", err)
		}
		return
	}

	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		log.Printf("[NOTIFY] Warning: could not parse notified-chapters file: %v", err)
		return
	}

	purged := 0
	for _, k := range keys {
		// Drop legacy h-manga chapter entries. Pre-archive builds stored
		// subdirectory book names as "chapters"; under the new policy those
		// don't represent anything that should suppress a future archive
		// notification for the same artist, so they must not be retained.
		// Archive entries are namespaced with a literal "archive" segment
		// and are never touched here.
		if strings.HasPrefix(k, SectionHManga+"|") && !strings.HasPrefix(k, SectionHManga+"|archive|") {
			log.Printf("[NOTIFY] Discarding legacy h-manga chapter entry: %s", k)
			purged++
			continue
		}
		t.notified[k] = true
	}
	log.Printf("[NOTIFY] Loaded %d previously notified entries from %s (purged %d legacy)", len(t.notified), t.filePath, purged)

	// If we purged anything, rewrite the file now so subsequent startups
	// don't reload and re-purge the same entries (and re-log them). The
	// marshal + temp-file write + rename is done under the same lock as
	// Save() so concurrent Mark/MarkArchive calls can't race the rewrite.
	if purged > 0 {
		t.saveLocked()
	}
}

// Save persists the full set of notified keys to disk. The lock is held
// through the entire marshal + temp-file write + rename so concurrent Mark
// calls cannot mutate the set mid-write.
func (t *NotificationTracker) Save() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saveLocked()
}

// saveLocked is the unlocked inner half of Save, factored out so Load
// can persist a post-purge state while already holding t.mu. Caller MUST
// hold t.mu.
func (t *NotificationTracker) saveLocked() {
	keys := make([]string, 0, len(t.notified))
	for k := range t.notified {
		keys = append(keys, k)
	}

	data, err := json.Marshal(keys)
	if err != nil {
		log.Printf("[NOTIFY] Warning: could not marshal notified-chapters: %v", err)
		return
	}

	dir := filepath.Dir(t.filePath)
	os.MkdirAll(dir, 0750)

	tmpPath := t.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		log.Printf("[NOTIFY] Warning: could not write notified-chapters: %v", err)
		return
	}
	if err := os.Rename(tmpPath, t.filePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("[NOTIFY] Warning: could not rename notified-chapters: %v", err)
	}
}

// Mark records chapters as having had a notification pushed.
func (t *NotificationTracker) Mark(section, series string, chapters []string) {
	t.mu.Lock()
	for _, ch := range chapters {
		t.notified[notifiedKey(section, series, ch)] = true
	}
	t.mu.Unlock()

	t.Save()
}

// MarkArchive records archive filenames as having had a notification pushed
// for the given artist in the given section. Keys are namespaced apart from
// chapter keys so the two never collide.
func (t *NotificationTracker) MarkArchive(section, artist string, archives []string) {
	t.mu.Lock()
	for _, a := range archives {
		t.notified[notifiedArchiveKey(section, artist, a)] = true
	}
	t.mu.Unlock()

	t.Save()
}

// IsNotified checks if a specific chapter has already had a notification pushed.
func (t *NotificationTracker) IsNotified(section, series, chapter string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.notified[notifiedKey(section, series, chapter)]
}

// IsArchiveNotified checks if a specific h-manga archive has already had a
// notification pushed for the given artist.
func (t *NotificationTracker) IsArchiveNotified(section, artist, archive string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.notified[notifiedArchiveKey(section, artist, archive)]
}

// GetPending returns all chapters in the DB that have NOT yet had
// notifications pushed. h-manga chapter entries are intentionally NOT
// returned — under the new policy only h-manga archive additions are
// notified, so there are no "pending chapters" for that section.
func (t *NotificationTracker) GetPending(db *InMemoryDB) []NewChapter {
	var pending []NewChapter

	for _, section := range []string{SectionManga, SectionHManga} {
		if section == SectionHManga {
			// h-manga is archive-driven; chapters are not notified. See
			// GetPendingArchives for the archive equivalent.
			continue
		}
		folders := db.GetMangaFolders(section)
		for _, series := range folders {
			for _, ch := range series.Chapters {
				if !t.IsNotified(section, series.Name, ch.Name) {
					pending = append(pending, NewChapter{
						Series:  series.Name,
						Chapter: ch.Name,
						Section: section,
					})
				}
			}
		}
	}

	return pending
}

// GetPendingArchives scans the h-manga root directory for archive files
// (.zip / .cbz / .rar / .7z) directly inside artist directories and
// returns those that have NOT yet been notified. The returned slice is
// flat (one entry per archive); grouping by artist is the caller's
// responsibility.
//
// Returns nil if the root directory cannot be read (snapshot failed) so
// the caller can distinguish "no pending archives" from "don't know, skip
// push" — preventing a transient network-drive failure from causing a
// mass notification when the drive recovers.
func (t *NotificationTracker) GetPendingArchives(hMangaRoot string) []NewArchive {
	snapshot := getHMangaArchiveSnapshot(hMangaRoot)
	if snapshot == nil {
		return nil
	}
	var pending []NewArchive
	for artist, archives := range snapshot {
		for _, a := range archives {
			if !t.IsArchiveNotified(SectionHManga, artist, a) {
				pending = append(pending, NewArchive{
					Artist:  artist,
					Archive: a,
					Section: SectionHManga,
				})
			}
		}
	}
	return pending
}
