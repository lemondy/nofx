package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type inputInjection struct {
	File       string `json:"file"`
	Label      string `json:"label"`
	RequireAll bool   `json:"require_all"`
	// Recomputed at load time; persisted in the variant snapshot and hash.
	SHA256 string `json:"sha256,omitempty"`
	rows   map[int64][]inputField
}

type inputField struct {
	Symbol string
	Text   string
}

func loadInputInject(v *variant, variantPath string, db *sql.DB) error {
	p := v.InputInject
	if p == nil {
		return nil
	}
	if p.File == "" || strings.TrimSpace(p.Label) == "" {
		return fmt.Errorf("input_inject 需要非空 file 和 label")
	}
	path := p.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(variantPath), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("input_inject 读取字段文件失败: %w", err)
	}
	fields := map[int64][]inputField{}
	type pair struct {
		recordID int64
		symbol   string
	}
	seen := map[pair]bool{}
	timestamps := map[int64]time.Time{}
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row struct {
			RecordID *int64  `json:"record_id"`
			Symbol   string  `json:"symbol"`
			Text     *string `json:"text"`
			AsOf     *string `json:"as_of"`
		}
		if err = json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("input_inject 第 %d 行无效: %w", i+1, err)
		}
		if row.RecordID == nil || strings.TrimSpace(row.Symbol) == "" || row.Text == nil {
			return fmt.Errorf("input_inject 第 %d 行需要 record_id、symbol 和 text", i+1)
		}
		id := *row.RecordID
		if utf8.RuneCountInString(*row.Text) > 300 {
			return fmt.Errorf("input_inject 第 %d 行记录 %d (%s) 的 text 超过 300 个字符", i+1, id, row.Symbol)
		}
		key := pair{id, row.Symbol}
		if seen[key] {
			return fmt.Errorf("input_inject 第 %d 行记录 %d (%s) 重复", i+1, id, row.Symbol)
		}
		seen[key] = true
		if row.AsOf != nil {
			asOf, err := time.Parse(time.RFC3339Nano, *row.AsOf)
			if err != nil {
				return fmt.Errorf("input_inject 记录 %d 的 as_of 不是有效 RFC3339 时间: %w", id, err)
			}
			decisionTime, ok := timestamps[id]
			if !ok {
				var ts any
				if err = db.QueryRow("SELECT timestamp FROM decision_records WHERE id=?", id).Scan(&ts); err != nil {
					return fmt.Errorf("input_inject 记录 %d 无法读取决策时间: %w", id, err)
				}
				decisionTime, err = databaseTime(ts)
				if err != nil || decisionTime.IsZero() {
					return fmt.Errorf("input_inject 记录 %d 的决策时间无效", id)
				}
				timestamps[id] = decisionTime
			}
			if asOf.After(decisionTime) {
				return fmt.Errorf("input_inject 记录 %d 的 as_of %s 晚于决策时间 %s", id, canonicalTime(asOf), canonicalTime(decisionTime))
			}
		}
		fields[id] = append(fields[id], inputField{row.Symbol, *row.Text})
	}
	for _, rows := range fields {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Symbol < rows[j].Symbol })
	}
	p.rows, p.SHA256 = fields, digest(data)
	return nil
}

var candidateHeader = regexp.MustCompile(`(?m)^## Candidate Coins \(`)

func (p *inputInjection) prepare(r record) (string, int, error) {
	rows := p.rows[r.ID]
	if len(rows) == 0 {
		if p.RequireAll {
			return "", 0, fmt.Errorf("patch_miss: input_inject 无该记录字段")
		}
		return "", 0, nil
	}
	hit := candidateHeader.FindStringIndex(r.Input)
	if hit == nil {
		hit = positionsHeader.FindStringIndex(r.Input)
	}
	if hit == nil {
		return "", 0, fmt.Errorf("patch_miss: input_inject 缺少候选币或持仓标题")
	}
	var block strings.Builder
	fmt.Fprintf(&block, "## %s\n", p.Label)
	for _, row := range rows {
		fmt.Fprintf(&block, "- %s: %s\n", row.Symbol, row.Text)
	}
	block.WriteByte('\n')
	return block.String(), hit[0], nil
}
