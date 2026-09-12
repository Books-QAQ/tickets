package kb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Store MySQL 侧（文本与元数据 = 真相源；向量不落 MySQL —— ADR-10）
type Store struct{ DB *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// UpsertDocument 幂等写文档，返回 doc_id
func (s *Store) UpsertDocument(ctx context.Context, doc Document) (int64, error) {
	var expire any
	if doc.ExpireAt != nil {
		expire = doc.ExpireAt.Format("2006-01-02 15:04:05")
	}
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO kb_documents
			(doc_key,title,category,doc_type,visibility,form,sub_scenario,capability,source,scope_kind,scope_ref,expire_at,content_hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			title=VALUES(title), category=VALUES(category), doc_type=VALUES(doc_type),
			visibility=VALUES(visibility), form=VALUES(form), sub_scenario=VALUES(sub_scenario),
			capability=VALUES(capability), source=VALUES(source), scope_kind=VALUES(scope_kind),
			scope_ref=VALUES(scope_ref), expire_at=VALUES(expire_at), content_hash=VALUES(content_hash),
			id=LAST_INSERT_ID(id)`,
		doc.DocKey, doc.Title, doc.Category, doc.DocType, doc.Visibility, string(doc.Form),
		nullIfEmpty(doc.SubScenario), string(doc.Capability), nullIfEmpty(doc.Source),
		doc.ScopeKind, nullIfEmpty(doc.ScopeRef), expire, doc.ContentHash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpsertChunk 幂等写块（唯一键 doc_id+seq），返回 chunk_id
func (s *Store) UpsertChunk(ctx context.Context, docID int64, c Chunk) (int64, error) {
	var expire any
	if c.ExpireAt != nil {
		expire = c.ExpireAt.Format("2006-01-02 15:04:05")
	}
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO kb_chunks
			(doc_id,seq,source_label,content,category,doc_type,form,sub_scenario,capability,visibility,
			 scope_kind,scope_ref,heading_path,expire_at,embedding_model,content_hash)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			source_label=VALUES(source_label), content=VALUES(content), category=VALUES(category),
			doc_type=VALUES(doc_type), form=VALUES(form), sub_scenario=VALUES(sub_scenario),
			capability=VALUES(capability), visibility=VALUES(visibility), scope_kind=VALUES(scope_kind),
			scope_ref=VALUES(scope_ref), heading_path=VALUES(heading_path), expire_at=VALUES(expire_at),
			embedding_model=VALUES(embedding_model), content_hash=VALUES(content_hash),
			id=LAST_INSERT_ID(id)`,
		docID, c.Seq, c.SourceLabel, c.Content, c.Category, c.DocType, string(c.Form),
		nullIfEmpty(c.SubScenario), string(c.Capability), c.Visibility, c.ScopeKind,
		nullIfEmpty(c.ScopeRef), nullIfEmpty(c.HeadingPath), expire, nullIfEmpty(c.EmbeddingModel), c.ContentHash)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetChunkEmbeddingModel 记录该块是用哪个模型嵌入的（与 Qdrant payload 同步，用于漂移对账）
func (s *Store) SetChunkEmbeddingModel(ctx context.Context, chunkID int64, model string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE kb_chunks SET embedding_model=? WHERE id=?`, model, chunkID)
	return err
}

// DeleteDocByKey 删除文档（级联删块）—— 源文件被删时用
func (s *Store) DeleteDocByKey(ctx context.Context, docKey string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM kb_documents WHERE doc_key=?`, docKey)
	return err
}

// DeleteExtraChunks 文档变短后清掉多余的块（seq > keepMax）
func (s *Store) DeleteExtraChunks(ctx context.Context, docID int64, keepMax int) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id=? AND seq>?`, docID, keepMax)
	return err
}

// ChunkHashIndex 返回 doc_key + "#" + seq -> content_hash（增量入库的游标）
func (s *Store) ChunkHashIndex(ctx context.Context) (map[string]string, map[string]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT d.doc_key, c.seq, c.content_hash, c.id
		FROM kb_chunks c JOIN kb_documents d ON d.id = c.doc_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	hashes := map[string]string{}
	ids := map[string]int64{}
	for rows.Next() {
		var docKey, hash string
		var seq int
		var id int64
		if err := rows.Scan(&docKey, &seq, &hash, &id); err != nil {
			return nil, nil, err
		}
		k := fmt.Sprintf("%s#%d", docKey, seq)
		hashes[k] = hash
		ids[k] = id
	}
	return hashes, ids, rows.Err()
}

// AllChunkMetas 重建 BM25 索引用：返回元数据 + 正文（正文只用于分词，建完索引即丢弃，不常驻内存）
func (s *Store) AllChunkMetas(ctx context.Context) ([]ChunkMeta, []string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, source_label, category, form, capability, visibility, scope_kind, COALESCE(scope_ref,''), COALESCE(sub_scenario,''), content
		FROM kb_chunks`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var metas []ChunkMeta
	var contents []string
	for rows.Next() {
		var m ChunkMeta
		var content, form, capability string
		if err := rows.Scan(&m.ChunkID, &m.Label, &m.Category, &form, &capability, &m.Visibility, &m.ScopeKind, &m.ScopeRef, &m.SubScenario, &content); err != nil {
			return nil, nil, err
		}
		m.Form = Form(form)
		m.Capability = Capability(capability)
		metas = append(metas, m)
		contents = append(contents, content)
	}
	return metas, contents, rows.Err()
}

// ChunksByIDs 取最终 top-k 的正文（先检索后取文本，索引里不存正文）
func (s *Store) ChunksByIDs(ctx context.Context, ids []int64) ([]Chunk, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	q := `SELECT id, doc_id, seq, source_label, content, category, doc_type, form, sub_scenario,
	             capability, visibility, scope_kind, COALESCE(scope_ref,''), COALESCE(heading_path,''),
	             expire_at, COALESCE(embedding_model,''), content_hash
	      FROM kb_chunks WHERE id IN (` + ph + `)`
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := map[int64]Chunk{}
	for rows.Next() {
		var c Chunk
		var form, capability, subScenario, scopeRef string
		var expire sql.NullTime
		if err := rows.Scan(&c.ID, &c.DocID, &c.Seq, &c.SourceLabel, &c.Content, &c.Category, &c.DocType,
			&form, &subScenario, &capability, &c.Visibility, &c.ScopeKind, &scopeRef, &c.HeadingPath,
			&expire, &c.EmbeddingModel, &c.ContentHash); err != nil {
			return nil, err
		}
		c.Form = Form(form)
		c.Capability = Capability(capability)
		c.SubScenario = subScenario
		c.ScopeRef = scopeRef
		if expire.Valid {
			t := expire.Time
			c.ExpireAt = &t
		}
		byID[c.ID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Chunk, 0, len(ids))
	for _, id := range ids { // 保持融合后的顺序
		if c, ok := byID[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// ChunksByLabel 引用校验用：按 source_label 反查块
func (s *Store) ChunksByLabel(ctx context.Context, labels []string) (map[string]Chunk, error) {
	if len(labels) == 0 {
		return map[string]Chunk{}, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(labels)), ",")
	q := `SELECT id, seq, source_label, content, category, form, capability, visibility,
	             scope_kind, COALESCE(scope_ref,''), COALESCE(heading_path,'')
	      FROM kb_chunks WHERE source_label IN (` + ph + `)`
	args := make([]any, 0, len(labels))
	for _, l := range labels {
		args = append(args, l)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Chunk{}
	for rows.Next() {
		var c Chunk
		var form, capability string
		if err := rows.Scan(&c.ID, &c.Seq, &c.SourceLabel, &c.Content, &c.Category, &form, &capability,
			&c.Visibility, &c.ScopeKind, &c.ScopeRef, &c.HeadingPath); err != nil {
			return nil, err
		}
		c.Form = Form(form)
		c.Capability = Capability(capability)
		out[c.SourceLabel] = c
	}
	return out, rows.Err()
}

// ScopeRefs 返回 scope_kind -> 去重 scope_ref（两级检索第一级的词表）
func (s *Store) ScopeRefs(ctx context.Context) (map[string][]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT scope_kind, scope_ref FROM kb_chunks
		WHERE scope_kind <> 'general' AND scope_ref IS NOT NULL AND scope_ref <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var kind, ref string
		if err := rows.Scan(&kind, &ref); err != nil {
			return nil, err
		}
		out[kind] = append(out[kind], ref)
	}
	return out, rows.Err()
}

func (s *Store) CountChunks(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks`).Scan(&n)
	return n, err
}

// —— 能力台账 ——

func (s *Store) UpsertCapability(ctx context.Context, row CapabilityRow) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO cs_capabilities (sub_scenario,category,status,system_entry,owner,target_milestone,source,note)
		VALUES (?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE category=VALUES(category), status=VALUES(status),
			system_entry=VALUES(system_entry), owner=VALUES(owner), target_milestone=VALUES(target_milestone),
			source=VALUES(source), note=VALUES(note)`,
		row.SubScenario, row.Category, string(row.Status), nullIfEmpty(row.SystemEntry),
		nullIfEmpty(row.Owner), nullIfEmpty(row.TargetMilestone), nullIfEmpty(row.Source), nullIfEmpty(row.Note))
	return err
}

func (s *Store) LoadCapabilities(ctx context.Context) (map[string]CapabilityRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT sub_scenario, category, status, COALESCE(system_entry,''), COALESCE(owner,''),
		       COALESCE(target_milestone,''), COALESCE(source,''), COALESCE(note,'')
		FROM cs_capabilities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CapabilityRow{}
	for rows.Next() {
		var r CapabilityRow
		var status string
		if err := rows.Scan(&r.SubScenario, &r.Category, &status, &r.SystemEntry, &r.Owner,
			&r.TargetMilestone, &r.Source, &r.Note); err != nil {
			return nil, err
		}
		r.Status = Capability(status)
		out[r.SubScenario] = r
	}
	return out, rows.Err()
}

// ChunkIDsBySubScenario 台账翻转时定位要改 payload 的 chunk（不重嵌）
func (s *Store) ChunkIDsBySubScenario(ctx context.Context, sub string) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM kb_chunks WHERE sub_scenario=?`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

var _ = time.Now
