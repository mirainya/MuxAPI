package store

import (
	"database/sql"
	"errors"

	"github.com/mirainya/muxapi/internal/upstream"
	"gorm.io/gorm"
)

// --- 上游全局池 ---

func (s *Store) ListTags() ([]upstream.Tag, error) {
	var models []TagModel
	if err := s.gormDB.Order("sort_order, id").Find(&models).Error; err != nil {
		return nil, err
	}
	tags := make([]upstream.Tag, len(models))
	for i, m := range models {
		tags[i] = upstream.Tag{ID: m.ID, Name: m.Name, Color: m.Color}
	}
	return tags, nil
}

func (s *Store) CreateTag(name, color string) (int64, error) {
	var id int64
	err := s.queryRow(`INSERT INTO tags(name,color,sort_order)
		VALUES(?,?,(SELECT COALESCE(MAX(sort_order),0)+1 FROM tags)) RETURNING id`, name, color).Scan(&id)
	return id, err
}

func (s *Store) UpdateTag(id int64, name, color string) error {
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE tags SET name=?,color=? WHERE id=?`, name, color, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE upstreams SET source=? WHERE id IN
		(SELECT upstream_id FROM upstream_tags WHERE tag_id=? AND is_primary=TRUE)`, name, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteTag(id int64) error {
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE upstreams SET source='' WHERE id IN
		(SELECT upstream_id FROM upstream_tags WHERE tag_id=? AND is_primary=TRUE)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tags WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) loadUpstreamTags(list []*upstream.Upstream) error {
	if len(list) == 0 {
		return nil
	}
	byID := make(map[int64]*upstream.Upstream, len(list))
	for _, item := range list {
		item.Tags = []upstream.Tag{}
		item.TagIDs = []int64{}
		byID[item.ID] = item
	}
	rows, err := s.query(`SELECT ut.upstream_id,t.id,t.name,t.color,ut.is_primary
		FROM upstream_tags ut JOIN tags t ON t.id=ut.tag_id ORDER BY t.sort_order,t.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var upstreamID int64
		var tag upstream.Tag
		if err := rows.Scan(&upstreamID, &tag.ID, &tag.Name, &tag.Color, &tag.IsPrimary); err != nil {
			return err
		}
		item := byID[upstreamID]
		if item == nil {
			continue
		}
		item.Tags = append(item.Tags, tag)
		item.TagIDs = append(item.TagIDs, tag.ID)
		if tag.IsPrimary {
			item.PrimaryTagID = tag.ID
		}
	}
	return rows.Err()
}

func replaceUpstreamTags(tx *rawTx, upstreamID, primaryTagID int64, tagIDs []int64) error {
	if _, err := tx.Exec(`DELETE FROM upstream_tags WHERE upstream_id=?`, upstreamID); err != nil {
		return err
	}
	ids := make(map[int64]bool, len(tagIDs)+1)
	for _, id := range tagIDs {
		if id > 0 {
			ids[id] = false
		}
	}
	if primaryTagID > 0 {
		ids[primaryTagID] = true
	}
	for id, primary := range ids {
		if _, err := tx.Exec(`INSERT INTO upstream_tags(upstream_id,tag_id,is_primary) VALUES(?,?,?)`, upstreamID, id, primary); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`UPDATE upstreams SET source=COALESCE((SELECT t.name FROM upstream_tags ut
		JOIN tags t ON t.id=ut.tag_id WHERE ut.upstream_id=? AND ut.is_primary=TRUE),'') WHERE id=?`, upstreamID, upstreamID)
	return err
}

// scanUps 扫描含组内视图 priority/weight 的行（JOIN 查询用）。
func scanUps(rows *sql.Rows) ([]*upstream.Upstream, error) {
	defer rows.Close()
	var list []*upstream.Upstream
	for rows.Next() {
		u := &upstream.Upstream{}
		if err := rows.Scan(&u.ID, &u.Name, &u.Source, &u.BaseURL, &u.APIKey, &u.Proxy, &u.Protocol, &u.BillingType, &u.CacheMode, &u.Enabled, &u.Priority, &u.Weight, &u.ChannelProbe, &u.CreditRatio); err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

// ListEnabledByGroup 返回某分组下启用的上游，JOIN 中间表填充组内 priority/weight，
// 按组内优先级升序（调度用）。
func (s *Store) ListEnabledByGroup(groupID int64) ([]*upstream.Upstream, error) {
	rows, err := s.query(`SELECT u.id,u.name,u.source,u.base_url,u.api_key,u.proxy,u.protocol,u.billing_type,u.cache_mode,u.enabled,gu.priority,gu.weight,u.channel_probe,u.credit_ratio
		FROM upstreams u JOIN group_upstreams gu ON gu.upstream_id=u.id
		JOIN groups g ON g.id=gu.group_id
		LEFT JOIN upstream_billing_status bs ON bs.upstream_id=u.id
		WHERE gu.group_id=? AND u.enabled=TRUE AND gu.enabled=TRUE
		AND (g.max_multiplier IS NULL OR COALESCE(bs.effective_multiplier,bs.group_multiplier) IS NULL
			OR COALESCE(bs.effective_multiplier,bs.group_multiplier)/u.credit_ratio<=g.max_multiplier)
		ORDER BY gu.priority ASC`, groupID)
	if err != nil {
		return nil, err
	}
	return scanUps(rows)
}

// List 返回全部上游(含停用)，priority/weight 置 0（全局池无组内语义），供探测与后台管理。
func (s *Store) List() ([]*upstream.Upstream, error) {
	rows, err := s.query(`SELECT id,name,source,base_url,api_key,proxy,protocol,billing_type,cache_mode,enabled,0,0,channel_probe,credit_ratio FROM upstreams ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	list, err := scanUps(rows)
	if err != nil {
		return nil, err
	}
	if err := s.loadUpstreamTags(list); err != nil {
		return nil, err
	}
	return list, nil
}

// Get 按 id 取单个上游（含完整 api_key，供连通测试用）。
func (s *Store) Get(id int64) (*upstream.Upstream, error) {
	u := &upstream.Upstream{}
	err := s.queryRow(`SELECT id,name,source,base_url,api_key,proxy,protocol,billing_type,cache_mode,enabled,channel_probe,credit_ratio FROM upstreams WHERE id=?`, id).
		Scan(&u.ID, &u.Name, &u.Source, &u.BaseURL, &u.APIKey, &u.Proxy, &u.Protocol, &u.BillingType, &u.CacheMode, &u.Enabled, &u.ChannelProbe, &u.CreditRatio)
	if err == nil {
		err = s.loadUpstreamTags([]*upstream.Upstream{u})
	}
	return u, err
}

func (s *Store) Create(u *upstream.Upstream) error {
	billingType, ok := upstream.NormalizeBillingType(u.BillingType)
	if !ok {
		return errors.New("unsupported billing type")
	}
	u.BillingType = billingType
	cacheMode, ok := upstream.NormalizeCacheMode(u.CacheMode)
	if !ok {
		return errors.New("unsupported cache mode")
	}
	u.CacheMode = cacheMode
	if u.CreditRatio <= 0 {
		u.CreditRatio = 1
	}
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRow(`INSERT INTO upstreams(name,source,base_url,api_key,proxy,protocol,billing_type,cache_mode,enabled,channel_probe,credit_ratio,sort_order)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(sort_order),0)+1 FROM upstreams)) RETURNING id`,
		u.Name, u.Source, u.BaseURL, u.APIKey, u.Proxy, u.Protocol, u.BillingType, u.CacheMode, u.Enabled, u.ChannelProbe, u.CreditRatio).Scan(&u.ID); err != nil {
		return err
	}
	if u.TagsSet {
		if err := replaceUpstreamTags(tx, u.ID, u.PrimaryTagID, u.TagIDs); err != nil {
			return err
		}
	}
	for _, gid := range u.GroupIDs {
		if _, err := tx.Exec(`INSERT INTO group_upstreams(group_id,upstream_id,priority,weight)
			VALUES(?,?,50,1) ON CONFLICT(group_id,upstream_id) DO NOTHING`, gid, u.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReorderUpstreams 按给定 id 顺序写入 sort_order 权重（从 1 起）。
func (s *Store) ReorderUpstreams(ids []int64) error {
	return s.gormDB.Transaction(func(tx *gorm.DB) error {
		for i, id := range ids {
			if err := tx.Model(&UpstreamModel{}).Where("id = ?", id).Update("sort_order", i+1).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Update(u *upstream.Upstream) error {
	billingType, ok := upstream.NormalizeBillingType(u.BillingType)
	if !ok {
		return errors.New("unsupported billing type")
	}
	u.BillingType = billingType
	cacheMode, ok := upstream.NormalizeCacheMode(u.CacheMode)
	if !ok {
		return errors.New("unsupported cache mode")
	}
	u.CacheMode = cacheMode
	if u.CreditRatio <= 0 {
		u.CreditRatio = 1
	}
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldBillingType, oldBaseURL, oldAPIKey string
	if err := tx.QueryRow(`SELECT billing_type,base_url,api_key FROM upstreams WHERE id=?`, u.ID).
		Scan(&oldBillingType, &oldBaseURL, &oldAPIKey); err != nil {
		return err
	}
	if u.APIKey == "" { // 留空则不改凭证（对齐后台「留空则不修改」语义）
		_, err = tx.Exec(`UPDATE upstreams SET name=?,source=?,base_url=?,proxy=?,protocol=?,billing_type=?,cache_mode=?,enabled=?,channel_probe=?,credit_ratio=? WHERE id=?`,
			u.Name, u.Source, u.BaseURL, u.Proxy, u.Protocol, u.BillingType, u.CacheMode, u.Enabled, u.ChannelProbe, u.CreditRatio, u.ID)
	} else {
		_, err = tx.Exec(`UPDATE upstreams SET name=?,source=?,base_url=?,api_key=?,proxy=?,protocol=?,billing_type=?,cache_mode=?,enabled=?,channel_probe=?,credit_ratio=? WHERE id=?`,
			u.Name, u.Source, u.BaseURL, u.APIKey, u.Proxy, u.Protocol, u.BillingType, u.CacheMode, u.Enabled, u.ChannelProbe, u.CreditRatio, u.ID)
	}
	if err != nil {
		return err
	}
	credentialChanged := oldBillingType != u.BillingType || oldBaseURL != u.BaseURL ||
		(u.APIKey != "" && oldAPIKey != u.APIKey)
	if credentialChanged {
		if _, err := tx.Exec(`DELETE FROM upstream_billing_snapshots WHERE upstream_id=?`, u.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM upstream_billing_status WHERE upstream_id=?`, u.ID); err != nil {
			return err
		}
	}
	if u.TagsSet {
		if err := replaceUpstreamTags(tx, u.ID, u.PrimaryTagID, u.TagIDs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type UpstreamBatchUpdate struct {
	Enabled      *bool
	PrimaryTagID *int64
	AddTagIDs    []int64
	RemoveTagIDs []int64
}

// BatchUpdateUpstreams updates management metadata without touching credentials or routing membership.
func (s *Store) BatchUpdateUpstreams(ids []int64, update UpstreamBatchUpdate) error {
	if len(ids) == 0 || (update.Enabled == nil && update.PrimaryTagID == nil && len(update.AddTagIDs) == 0 && len(update.RemoveTagIDs) == 0) {
		return errors.New("batch update requires ids and at least one field")
	}
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if id <= 0 {
			return errors.New("invalid upstream id")
		}
		if update.Enabled != nil {
			if _, err := tx.Exec(`UPDATE upstreams SET enabled=? WHERE id=?`, *update.Enabled, id); err != nil {
				return err
			}
		}
		for _, tagID := range update.AddTagIDs {
			if tagID <= 0 {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO upstream_tags(upstream_id,tag_id,is_primary) VALUES(?,?,FALSE)
				ON CONFLICT(upstream_id,tag_id) DO NOTHING`, id, tagID); err != nil {
				return err
			}
		}
		for _, tagID := range update.RemoveTagIDs {
			if _, err := tx.Exec(`DELETE FROM upstream_tags WHERE upstream_id=? AND tag_id=? AND is_primary=FALSE`, id, tagID); err != nil {
				return err
			}
		}
		if update.PrimaryTagID != nil {
			if _, err := tx.Exec(`UPDATE upstream_tags SET is_primary=FALSE WHERE upstream_id=?`, id); err != nil {
				return err
			}
			if *update.PrimaryTagID > 0 {
				if _, err := tx.Exec(`INSERT INTO upstream_tags(upstream_id,tag_id,is_primary) VALUES(?,?,TRUE)
					ON CONFLICT(upstream_id,tag_id) DO UPDATE SET is_primary=TRUE`, id, *update.PrimaryTagID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE upstreams SET source=COALESCE((SELECT t.name FROM upstream_tags ut
				JOIN tags t ON t.id=ut.tag_id WHERE ut.upstream_id=? AND ut.is_primary=TRUE),'') WHERE id=?`, id, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) Delete(id int64) error {
	return s.gormDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("upstream_id = ?", id).Delete(&GroupUpstreamModel{}).Error; err != nil {
			return err
		}
		if err := tx.Where("upstream_id = ?", id).Delete(&MonitorModel{}).Error; err != nil {
			return err
		}
		if err := tx.Where("upstream_id = ?", id).Delete(&ModelExclusion{}).Error; err != nil {
			return err
		}
		return tx.Delete(&UpstreamModel{}, id).Error
	})
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.requestQueue) })
	<-s.requestDone
	return s.sqlDB.Close()
}
