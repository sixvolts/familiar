package db

// FactHashSQL is memory.FactHash as a SQL expression over a memories
// row aliased m: the content_hash behind the (agent_id, content_hash)
// dedup, covering owner, content, scope tag and, for wiki_page rows,
// the owning page. Anything that changes one of those columns in SQL
// must rewrite content_hash with it (RehashFactsSQL), or the row keeps
// its old identity. The two must stay in step;
// memory.TestMigrate_WikiFactsKeyedByPageID pins that they agree.
const FactHashSQL = `encode(sha256(
	convert_to(COALESCE(m.user_id, ''), 'UTF8') || '\x00'::bytea
	|| convert_to(m.content, 'UTF8')
	|| CASE WHEN COALESCE(m.scope_tag, '') <> ''
	        THEN '\x00'::bytea || convert_to('scope:' || m.scope_tag, 'UTF8')
	        ELSE ''::bytea END
	|| CASE WHEN m.source_type = 'wiki_page' AND COALESCE(m.source_ref, '') <> ''
	        THEN '\x00'::bytea || convert_to('src:' || m.source_ref, 'UTF8')
	        ELSE ''::bytea END
), 'hex')`

// RehashFactsSQL rewrites content_hash to FactHashSQL for the memories
// rows matching where (a predicate over m). A row whose new hash
// another row already holds keeps its old one, since the unique index
// would refuse it: it is a duplicate of that row either way.
func RehashFactsSQL(where string) string {
	return `
	WITH rehash AS (
	    SELECT m.id, m.agent_id, ` + FactHashSQL + ` AS h
	      FROM memories m
	     WHERE ` + where + `
	), ranked AS (
	    SELECT r.*, row_number() OVER (PARTITION BY r.agent_id, r.h ORDER BY r.id) AS rn
	      FROM rehash r
	)
	UPDATE memories m
	   SET content_hash = r.h
	  FROM ranked r
	 WHERE m.id = r.id
	   AND r.rn = 1
	   AND m.content_hash IS DISTINCT FROM r.h
	   AND NOT EXISTS (SELECT 1 FROM memories o
	                    WHERE o.agent_id = r.agent_id AND o.content_hash = r.h AND o.id <> r.id)`
}
