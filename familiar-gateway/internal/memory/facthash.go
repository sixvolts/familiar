package memory

import (
	"crypto/sha256"
	"encoding/hex"
)

// FactHash is the content_hash behind the (agent_id, content_hash)
// write-time dedup: two commits with the same hash are the same fact,
// and the second one lands on the first one's row.
//
// So the hash has to cover everything that makes two rows different
// facts, not just their text:
//
//   - the owner, so byte-identical content from two users can't merge
//     onto one row (the old cross-tenant dedup bug);
//   - the scope tag, so an isolated shard's fact or a book's wiki fact
//     never merges onto the owner's top-level row (and the upsert can
//     never retag it);
//   - for wiki_page rows, the page that owns them, because a page save
//     deletes and rewrites its own rows. When two pages, or a page and
//     an explicit `remember`, shared one row, saving one page deleted
//     the other's knowledge.
//
// Unscoped non-wiki rows hash exactly as before (owner NUL content), so
// every existing top-level row still dedups against new commits. The
// wiki_page_fact_identity migration and shard retagging rewrite hashes
// in SQL (db.FactHashSQL), so that expression must stay in step with
// this function; TestMigrate_WikiFactsKeyedByPageID pins that they
// agree. Content can't contain NUL (Postgres TEXT rejects it), so the
// NUL-separated segments can't be forged from inside the content.
func FactHash(userID, scopeTag, sourceType, sourceRef, content string) string {
	key := userID + "\x00" + content
	if scopeTag != "" {
		key += "\x00scope:" + scopeTag
	}
	if sourceType == "wiki_page" && sourceRef != "" {
		key += "\x00src:" + sourceRef
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
